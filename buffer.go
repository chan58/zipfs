// Copyright 2013-2018 C Hansen
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

package zipfs

import "sync"

type buffer [32768]byte

var bufPool struct {
	Get  func() *buffer // Allocate a buffer
	Free func(*buffer)  // Free the buffer
}

func init() {
	var pool sync.Pool

	bufPool.Get = func() *buffer {
		b, ok := pool.Get().(*buffer)
		if !ok {
			b = new(buffer)
		}
		return b
	}

	bufPool.Free = func(b *buffer) {
		if b != nil {
			pool.Put(b)
		}
	}
}
