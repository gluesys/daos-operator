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
	"crypto/sha256"
	"encoding/hex"
	"strings"
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

// jobName builds a Job name that Kubernetes accepts. The name also becomes the
// job-name label on the pod template, and labels stop at 63 characters, so a
// long owner (a PV name is 40 characters by itself, in a namespace) is shortened
// and made unique with a hash of the full key.
func jobName(prefix, key, op string) string {
	full := prefix + "-" + strings.ReplaceAll(key, "/", "-") + "-" + op
	if len(full) <= 63 {
		return full
	}
	h := sha256.Sum256([]byte(key))
	id := hex.EncodeToString(h[:4])
	keep := 63 - (len(prefix) + 1 + len(id) + 1 + len(op) + 1)
	short := strings.ReplaceAll(key, "/", "-")
	if keep < 1 {
		return prefix + "-" + id + "-" + op
	}
	if len(short) > keep {
		short = strings.TrimRight(short[:keep], "-")
	}
	return prefix + "-" + short + "-" + id + "-" + op
}
