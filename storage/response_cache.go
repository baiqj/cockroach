// Copyright 2014 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied.  See the License for the specific language governing
// permissions and limitations under the License. See the AUTHORS file
// for names of contributors.
//
// Author: Spencer Kimball (spencer.kimball@gmail.com)

package storage

import (
	"bytes"
	"fmt"
	"sync"

	"github.com/cockroachdb/cockroach/encoding"
)

// A ResponseCache provides idempotence for request retries. Each
// request to a range specifies a ClientCmdID in the request header
// which uniquely identifies a client command. After commands have
// been replicated via Raft, they are executed against the state
// machine and the results are stored in the ResponseCache.
//
// The ResponseCache stores responses in the underlying engine, using
// keys derived from KeyLocalRangeResponseCachePrefix, range ID and
// the ClientCmdID.
//
// A ResponseCache is safe for concurrent access.
type ResponseCache struct {
	rangeID  int64
	engine   Engine
	inflight map[ClientCmdID]*sync.Cond
	sync.Mutex
}

// NewResponseCache returns a new response cache. Every range replica
// maintains a response cache, not just the leader. However, when a
// replica loses or gains leadership of the Raft consensus group, the
// inflight map should be cleared.
func NewResponseCache(rangeID int64, engine Engine) *ResponseCache {
	return &ResponseCache{
		rangeID:  rangeID,
		engine:   engine,
		inflight: make(map[ClientCmdID]*sync.Cond),
	}
}

// ClearInflight removes all pending commands from the inflight map,
// signaling and clearing any inflight waiters.
func (rc *ResponseCache) ClearInflight() {
	rc.Lock()
	defer rc.Unlock()
	for _, cond := range rc.inflight {
		cond.Broadcast()
	}
	rc.inflight = map[ClientCmdID]*sync.Cond{}
}

// GetResponse looks up a response matching the specified cmdID and
// returns true if found. The response is deserialized into the
// supplied reply parameter. If no response is found, returns
// false. If a command is pending already for the cmdID, then this
// method will block until the the command is completed or the
// response cache is cleared.
func (rc *ResponseCache) GetResponse(cmdID ClientCmdID, reply interface{}) (bool, error) {
	// Do nothing if command ID is empty.
	if cmdID.IsEmpty() {
		return false, nil
	}
	rc.Lock()
	defer rc.Unlock()
	// If the command is inflight, wait for it to complete.
	for {
		if cond, ok := rc.inflight[cmdID]; ok {
			cond.Wait()
		} else {
			break
		}
	}
	// If the response is in the cache or we experienced an error, return.
	if ok, err := getI(rc.engine, rc.makeKey(cmdID), reply); ok || err != nil {
		return ok, err
	}
	// There's no command result cached for this ID; add inflight.
	rc.addInflightLocked(cmdID)
	return false, nil
}

// PutResponse writes a response to the cache for the specified cmdID.
// The inflight entry corresponding to cmdID is removed from the
// inflight map. Any requests waiting on the outcome of the inflight
// command will be signaled to wakeup and read the command response
// from the cache.
func (rc *ResponseCache) PutResponse(cmdID ClientCmdID, reply interface{}) error {
	// Do nothing if command ID is empty.
	if cmdID.IsEmpty() {
		return nil
	}
	rc.Lock()
	defer rc.Unlock()
	// Write the response value to the engine.
	key := rc.makeKey(cmdID)
	err := putI(rc.engine, key, reply)
	// Even on error, we remove the entry from the inflight map.
	if cond, ok := rc.inflight[cmdID]; ok {
		cond.Broadcast()
		delete(rc.inflight, cmdID)
	}
	return err
}

// addInflightLocked adds the supplied ClientCmdID to the inflight
// map. Any subsequent invocations of GetResponse for the same client
// command will block on the inflight cond var until either the
// response cache is cleared or this command is removed via
// PutResponse().
func (rc *ResponseCache) addInflightLocked(cmdID ClientCmdID) {
	if _, ok := rc.inflight[cmdID]; ok {
		panic(fmt.Sprintf("command %+v is already inflight; GetResponse() should have been "+
			"invoked first", cmdID))
	}
	rc.inflight[cmdID] = sync.NewCond(&rc.Mutex)
}

// makeKey encodes the range ID and client command ID into a key
// for storage in the underlying engine. Note that the prefix for
// response cache keys sorts them at the very top of the engine's
// keyspace.
// TODO(spencer): going to need to encode the server timestamp
//   for when the value was written for GC.
func (rc *ResponseCache) makeKey(cmdID ClientCmdID) Key {
	return Key(bytes.Join([][]byte{
		KeyLocalRangeResponseCachePrefix,
		encoding.EncodeInt(rc.rangeID),
		encoding.EncodeInt(cmdID.WallTime), // wall time helps sort for locality
		encoding.EncodeInt(cmdID.Random),
	}, []byte{}))
}