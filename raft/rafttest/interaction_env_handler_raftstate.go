// Copyright 2019 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package rafttest

import (
	"fmt"
	"go.etcd.io/etcd/v3/raft"
)

// Checks whether node id is in the voter list within st
func isVoter(id uint64, st raft.Status) bool {
	idMap := st.Config.Voters.IDs()
	for idx := range idMap {
		if id == idx {
			return true
		}
	}
	return false
}

// RaftState pretty-prints the raft state for all nodes to the output buffer.
// For each node, the information is based on its own configuration view.
func (env *InteractionEnv) handleRaftState() error {
	for idx, n := range env.Nodes {
		st := n.Status()
		var voterStatus string
		if isVoter(uint64(idx + 1), st) {
			voterStatus = "(Voter)"
		} else {
			voterStatus = "(Non-Voter)"
		}
		fmt.Fprintln(env.Output, idx + 1, ": ", st.RaftState.String(), voterStatus)
	}
	return nil
}
