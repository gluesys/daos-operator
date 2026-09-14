/*
SPDX-License-Identifier: Apache-2.0
Copyright 2026 Gluesys Co., Ltd.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"sync"
	"time"
)

// Probe cadence, in memory and per object key. A finished dmg/daos Job is
// deleted, and that delete event would reconcile the owner straight into the
// next Job; the hold keeps probes at their intended interval. A restart of the
// operator simply probes once more.
var (
	probeMu   sync.Mutex
	probeNext = map[string]time.Time{}
)

// holdFor returns how long to wait before the next probe Job for key may start.
func holdFor(key string) time.Duration {
	probeMu.Lock()
	defer probeMu.Unlock()
	if t, ok := probeNext[key]; ok {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

func setHold(key string, interval time.Duration) {
	probeMu.Lock()
	probeNext[key] = time.Now().Add(interval)
	probeMu.Unlock()
}

func clearHold(key string) {
	probeMu.Lock()
	delete(probeNext, key)
	probeMu.Unlock()
}
