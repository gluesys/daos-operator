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
	"context"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"strings"
	"testing"

	daosv1alpha1 "github.com/gluesys/daos-operator/api/v1alpha1"
)

// The gate exists to refuse a reintegrate that would only time out. It must not
// refuse the one case reintegrate is for: a rank that DAOS excluded and whose
// engine is back. The rank stays "excluded" until something reintegrates it, so
// gating on the rank state alone makes the documented recovery -- restart the
// server pod, then reintegrate -- impossible to finish (seen on cxl2, 2026-10-02).
func TestRankOpPrecondition(t *testing.T) {
	st := func(state, node string, ready bool) *daosv1alpha1.DaosSystemStatus {
		return &daosv1alpha1.DaosSystemStatus{
			Ranks:       []daosv1alpha1.RankStatus{{Rank: 1, Node: node, State: state}},
			NodeConfigs: []daosv1alpha1.NodeConfigStatus{{Node: node, ServerReady: ready}},
		}
	}

	cases := []struct {
		name, op, state string
		ready           bool
		wantRefusal     string // "" means it must pass the gate
	}{
		{"excluded, engine back -> allowed", "reintegrate", "excluded", true, ""},
		{"errored, engine back -> allowed", "reintegrate", "errored", true, ""},
		{"excluded, pod not ready -> refused", "reintegrate", "excluded", false, "no running engine"},
		{"awaitformat, pod not ready -> refused", "reintegrate", "awaitformat", false, "no running engine"},
		{"adminexcluded -> refused even with the engine up", "reintegrate", "adminexcluded", true, "administratively excluded"},
		{"joined -> allowed", "reintegrate", "joined", true, ""},
		{"other ops are never gated", "drain", "excluded", false, ""},
	}
	for _, c := range cases {
		got := rankOpPrecondition(c.op, "1", st(c.state, "n1", c.ready))
		switch {
		case c.wantRefusal == "" && got != "":
			t.Errorf("%s: expected the gate to pass, got refusal %q", c.name, got)
		case c.wantRefusal != "" && !strings.Contains(got, c.wantRefusal):
			t.Errorf("%s: expected a refusal mentioning %q, got %q", c.name, c.wantRefusal, got)
		}
	}

	// A rank not named by the request is not consulted at all.
	s := st("excluded", "n1", false)
	s.Ranks = append(s.Ranks, daosv1alpha1.RankStatus{Rank: 2, Node: "n2", State: "joined"})
	if got := rankOpPrecondition("reintegrate", "2", s); got != "" {
		t.Errorf("rank 2 requested, rank 1 unhealthy: expected pass, got %q", got)
	}
}

// The reconciler rebuilds the whole status from the DaosSystem it read at the start and
// replaces it at the end. Read from the informer cache, that copy can be older than the
// status the previous reconcile just wrote, and the write then moves fields backwards: on
// the CI cluster lastRankOp went from a finished reintegrate back to the earlier
// clear-exclude, and a finished exclude stayed "is running" (2026-10-03). The object must be
// read uncached.
func TestReconcileReadsTheSystemUncached(t *testing.T) {
	ctx := context.Background()
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = daosv1alpha1.AddToScheme(s)
	stale := &daosv1alpha1.DaosSystem{ObjectMeta: metav1.ObjectMeta{Name: "d1"},
		Spec: daosv1alpha1.DaosSystemSpec{NodeSelector: map[string]string{"role": "none"}}}
	stale.Status.LastRankOp = &daosv1alpha1.RankOpStatus{Op: "clear-exclude", Ranks: "0", Succeeded: true}
	fresh := stale.DeepCopy()
	fresh.Status.LastRankOp = &daosv1alpha1.RankOpStatus{Op: "reintegrate", Ranks: "0", Succeeded: true}

	cached := fake.NewClientBuilder().WithScheme(s).WithObjects(stale).WithStatusSubresource(stale).Build()
	api := fake.NewClientBuilder().WithScheme(s).WithObjects(fresh).WithStatusSubresource(fresh).Build()
	r := &DaosSystemReconciler{Client: api, APIReader: api, Scheme: s}
	r.Client = &readFrom{Client: api, reader: cached} // reads hit the "cache", writes the API

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "d1"}}); err != nil {
		t.Fatal(err)
	}
	got := &daosv1alpha1.DaosSystem{}
	if err := api.Get(ctx, types.NamespacedName{Name: "d1"}, got); err != nil {
		t.Fatal(err)
	}
	if got.Status.LastRankOp == nil || got.Status.LastRankOp.Op != "reintegrate" {
		t.Fatalf("status moved backwards to the cached copy: lastRankOp=%+v", got.Status.LastRankOp)
	}
}

// readFrom serves Get/List from reader (a stale cache) and everything else from Client.
type readFrom struct {
	client.Client
	reader client.Reader
}

func (r *readFrom) Get(ctx context.Context, k client.ObjectKey, o client.Object, opts ...client.GetOption) error {
	return r.reader.Get(ctx, k, o, opts...)
}

func (r *readFrom) List(ctx context.Context, l client.ObjectList, opts ...client.ListOption) error {
	return r.reader.List(ctx, l, opts...)
}
