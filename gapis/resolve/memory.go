// Copyright (C) 2017 Google Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package resolve

import (
	"context"

	"github.com/google/gapid/core/math/interval"
	"github.com/google/gapid/gapis/api"
	"github.com/google/gapid/gapis/api/sync"
	"github.com/google/gapid/gapis/capture"
	"github.com/google/gapid/gapis/memory"
	"github.com/google/gapid/gapis/messages"
	"github.com/google/gapid/gapis/service"
	"github.com/google/gapid/gapis/service/path"
)

// Memory resolves and returns the memory from the path p.
func Memory(ctx context.Context, p *path.Memory) (*service.Memory, error) {
	ctx = capture.Put(ctx, path.FindCapture(p))

	cmdIdx := p.After.Indices[0]
	fullCmdIdx := p.After.Indices

	allCmds, err := Cmds(ctx, path.FindCapture(p))
	if err != nil {
		return nil, err
	}

	sd, err := SyncData(ctx, path.FindCapture(p))
	if err != nil {
		return nil, err
	}

	cmds, err := sync.MutationCmdsFor(ctx, path.FindCapture(p), sd, allCmds, api.CmdID(cmdIdx), fullCmdIdx[1:])
	if err != nil {
		return nil, err
	}

	s, err := capture.NewState(ctx)
	if err != nil {
		return nil, err
	}

	shouldRecord := false
	r := memory.Range{Base: p.Address, Size: p.Size}
	var reads, writes, observed memory.RangeList
	s.Memory.SetOnCreate(func(id memory.PoolID, pool *memory.Pool) {
		if id == memory.PoolID(p.Pool) {
			pool.OnRead = func(rng memory.Range) {
				if shouldRecord && rng.Overlaps(r) {
					interval.Merge(&reads, rng.Window(r).Span(), false)
				}
			}
			pool.OnWrite = func(rng memory.Range) {
				if shouldRecord && rng.Overlaps(r) {
					interval.Merge(&writes, rng.Window(r).Span(), false)
				}
			}
		}
	})

	err = api.ForeachCmd(ctx, cmds[:len(cmds)-1], func(ctx context.Context, id api.CmdID, cmd api.Cmd) error {
		cmd.Mutate(ctx, id, s, nil)
		return nil
	})
	if err != nil {
		return nil, err
	}

	lastCmd := cmds[len(cmds)-1]
	if syncedApi, ok := lastCmd.API().(sync.SynchronizedAPI); len(fullCmdIdx) > 1 && ok {
		requestSubCmdIdx := api.SubCmdIdx(fullCmdIdx)
		syncedApi.MutateSubcommands(ctx, api.CmdID(cmdIdx), lastCmd, s,
			func(s *api.GlobalState, subCommandIndex api.SubCmdIdx, cmd api.Cmd) {
				// Turn on OnRead and OnWrite if the subcommand to be executed is or
				// contained by the requested subcommand.
				shouldRecord = requestSubCmdIdx.Contains(subCommandIndex)
			}, // preSubCmdCallback
			func(s *api.GlobalState, subCommandIndex api.SubCmdIdx, cmd api.Cmd) {
				// Turn off OnRead and OnWrite after each subcommand.
				shouldRecord = false
			}, //postSubCmdCallback
		)
	} else {
		shouldRecord = true
		api.MutateCmds(ctx, s, nil, lastCmd)
	}

	// Check whether the requested pool was ever created.
	pool, err := s.Memory.Get(memory.PoolID(p.Pool))
	if err != nil {
		return nil, &service.ErrDataUnavailable{Reason: messages.ErrInvalidMemoryPool(p.Pool)}
	}

	slice := pool.Slice(r)

	if !p.ExcludeObserved {
		observed = slice.ValidRanges()
	}

	var data []byte
	if !p.ExcludeData && slice.Size() > 0 {
		data = make([]byte, slice.Size())
		if err := slice.Get(ctx, 0, data); err != nil {
			return nil, err
		}
	}

	return &service.Memory{
		Data:     data,
		Reads:    service.NewMemoryRanges(reads),
		Writes:   service.NewMemoryRanges(writes),
		Observed: service.NewMemoryRanges(observed),
	}, nil
}