// Copyright Contributors to Agones a Series of LF Projects, LLC.
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

package gameservers

import (
	"context"
	"testing"
	"time"

	"agones.dev/agones/pkg/apis"
	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"agones.dev/agones/pkg/client/clientset/versioned/fake"
	"agones.dev/agones/pkg/client/informers/externalversions"
	"agones.dev/agones/pkg/util/runtime"
	"github.com/heptiolabs/healthcheck"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	k8sinformers "k8s.io/client-go/informers"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

type noopAPIHooks struct{}

func (noopAPIHooks) ValidateGameServerSpec(_ *agonesv1.GameServerSpec, _ *field.Path) field.ErrorList {
	return field.ErrorList{}
}
func (noopAPIHooks) ValidateScheduling(_ apis.SchedulingStrategy, _ *field.Path) field.ErrorList {
	return field.ErrorList{}
}
func (noopAPIHooks) MutateGameServerPod(_ *agonesv1.GameServerSpec, _ *corev1.Pod) error { return nil }
func (noopAPIHooks) SetEviction(_ *agonesv1.Eviction, _ *corev1.Pod) error               { return nil }

const (
	testNamespace   = "default"
	testGSName      = "test-game-server"
	everyMinuteCron = "* * * * *" // window always open when anchor is in past
	neverCron       = "0 0 1 1 *" // Jan 1st only — window never opens in tests
)

func enableRestartFeatureGate(t *testing.T) {
	t.Helper()
	// Use typed constant now that it is registered in features.go:
	//   FeatureGameServerScheduledRestart Feature = "GameServerScheduledRestart"
	err := runtime.ParseFeatures(string(runtime.FeatureGameServerScheduledRestart) + "=true")
	require.NoError(t, err,
		"Failed to enable %s — did you add it to featureDefaults in features.go?",
		runtime.FeatureGameServerScheduledRestart)
	t.Cleanup(func() {
		_ = runtime.ParseFeatures(string(runtime.FeatureGameServerScheduledRestart) + "=false")
	})
}

func newTestRestartController(t *testing.T) (
	*RestartController,
	*fake.Clientset,
	externalversions.SharedInformerFactory,
) {
	t.Helper()
	enableRestartFeatureGate(t)

	fakeAgonesClient := fake.NewSimpleClientset()
	fakeKubeClient := k8sfake.NewSimpleClientset()
	kubeInformerFactory := k8sinformers.NewSharedInformerFactory(fakeKubeClient, 0)
	agonesInformerFactory := externalversions.NewSharedInformerFactory(fakeAgonesClient, 0)

	c := NewRestartController(
		healthcheck.NewHandler(),
		fakeKubeClient,
		fakeAgonesClient,
		kubeInformerFactory,
		agonesInformerFactory,
	)
	return c, fakeAgonesClient, agonesInformerFactory
}

func newReadyGS(rp *agonesv1.RestartPolicy) *agonesv1.GameServer {
	return &agonesv1.GameServer{
		ObjectMeta: metav1.ObjectMeta{
			Name:              testGSName,
			Namespace:         testNamespace,
			CreationTimestamp: metav1.Now(),
			Annotations:       map[string]string{},
		},
		Spec: agonesv1.GameServerSpec{
			RestartPolicy: rp,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "game-server", Image: "us-docker.pkg.dev/agones-images/examples/simple-game-server:0.35"},
					},
				},
			},
		},
		Status: agonesv1.GameServerStatus{
			State: agonesv1.GameServerStateReady,
		},
	}
}

func seedLister(t *testing.T, factory externalversions.SharedInformerFactory, gs *agonesv1.GameServer) {
	t.Helper()
	err := factory.Agones().V1().GameServers().Informer().GetStore().Add(gs)
	require.NoError(t, err, "failed to seed GS into informer store")
}

func lastUpdateGS(t *testing.T, fakeClient *fake.Clientset) *agonesv1.GameServer {
	t.Helper()
	actions := fakeClient.Actions()
	for i := len(actions) - 1; i >= 0; i-- {
		a := actions[i]
		if a.GetVerb() != "update" || a.GetResource().Resource != "gameservers" {
			continue
		}
		if ua, ok := a.(k8stesting.UpdateAction); ok {
			if gs, ok2 := ua.GetObject().(*agonesv1.GameServer); ok2 {
				return gs
			}
		}
	}
	return nil
}

func TestNoRestartBeforeWindow(t *testing.T) {
	c, fakeClient, factory := newTestRestartController(t)

	gs := newReadyGS(&agonesv1.RestartPolicy{
		Schedule: neverCron, // next fire = Jan 1st, always in future
	})
	// Anchor = creation time (now) → nextRestart = future Jan 1st → now.Before(nextRestart)=true
	gs.CreationTimestamp = metav1.Now()

	seedLister(t, factory, gs)
	_, err := fakeClient.AgonesV1().GameServers(testNamespace).Create(
		context.Background(), gs, metav1.CreateOptions{},
	)
	require.NoError(t, err)

	err = c.reconcileRestart(context.Background(), gs)
	require.NoError(t, err)

	// updateNextRestartTime was called → one Update action on gameservers
	updated := lastUpdateGS(t, fakeClient)
	require.NotNil(t, updated, "expected one Update call to write next-restart annotation")

	_, pendingSet := updated.Annotations[agonesv1.GameServerRestartPendingSinceAnnotation]
	assert.False(t, pendingSet,
		"restart-pending-since must NOT be set before the window opens")

	_, nextSet := updated.Annotations[agonesv1.GameServerNextRestartAnnotation]
	assert.True(t, nextSet,
		"next-restart annotation must be written so the controller can track the upcoming window")
}

func TestRestartWhenIdle(t *testing.T) {
	c, fakeClient, factory := newTestRestartController(t)

	gs := newReadyGS(&agonesv1.RestartPolicy{Schedule: everyMinuteCron})

	pastAnchor := time.Now().UTC().Add(-2 * time.Minute)
	gs.Annotations[agonesv1.GameServerNextRestartAnnotation] = pastAnchor.Format(time.RFC3339)
	gs.Status.State = agonesv1.GameServerStateReady
	gs.Status.Players = nil

	seedLister(t, factory, gs)
	_, err := fakeClient.AgonesV1().GameServers(testNamespace).Create(
		context.Background(), gs, metav1.CreateOptions{},
	)
	require.NoError(t, err)

	err = c.reconcileRestart(context.Background(), gs)
	require.NoError(t, err)

	updated := lastUpdateGS(t, fakeClient)
	require.NotNil(t, updated, "expected an Update call (advanceAnchor after idle restart)")

	nextAnnotation, ok := updated.Annotations[agonesv1.GameServerNextRestartAnnotation]
	assert.True(t, ok, "next-restart annotation must still be present after restart")

	advancedTime, parseErr := time.Parse(time.RFC3339, nextAnnotation)
	require.NoError(t, parseErr)
	assert.True(t, advancedTime.After(pastAnchor),
		"next-restart annotation must be advanced past the old window")

	_, stillPending := updated.Annotations[agonesv1.GameServerRestartPendingSinceAnnotation]
	assert.False(t, stillPending, "restart-pending-since must be cleared after successful restart")
}

func TestDeferWhenAllocated(t *testing.T) {
	c, fakeClient, factory := newTestRestartController(t)

	gs := newReadyGS(&agonesv1.RestartPolicy{Schedule: everyMinuteCron})

	pastAnchor := time.Now().UTC().Add(-2 * time.Minute)
	gs.Annotations[agonesv1.GameServerNextRestartAnnotation] = pastAnchor.Format(time.RFC3339)
	gs.Status.State = agonesv1.GameServerStateAllocated

	seedLister(t, factory, gs)
	_, err := fakeClient.AgonesV1().GameServers(testNamespace).Create(
		context.Background(), gs, metav1.CreateOptions{},
	)
	require.NoError(t, err)

	err = c.reconcileRestart(context.Background(), gs)
	require.NoError(t, err)

	updated := lastUpdateGS(t, fakeClient)
	require.NotNil(t, updated, "expected an Update call to annotate restart-pending-since")

	pendingSince, ok := updated.Annotations[agonesv1.GameServerRestartPendingSinceAnnotation]
	assert.True(t, ok, "restart-pending-since must be set when restart is deferred")

	pendingTime, parseErr := time.Parse(time.RFC3339, pendingSince)
	require.NoError(t, parseErr)
	assert.WithinDuration(t, time.Now().UTC(), pendingTime, 5*time.Second,
		"restart-pending-since must record approximately the current time")
}

func TestSoftDeadlineSkip(t *testing.T) {
	softDeadline := metav1.Duration{Duration: 1 * time.Hour}
	c, fakeClient, factory := newTestRestartController(t)

	gs := newReadyGS(&agonesv1.RestartPolicy{
		Schedule:             everyMinuteCron,
		SoftDeadlineDuration: &softDeadline,
	})

	windowOpenedAt := time.Now().UTC().Add(-2 * time.Minute)
	gs.Annotations[agonesv1.GameServerNextRestartAnnotation] = windowOpenedAt.Format(time.RFC3339)

	pendingSinceTime := time.Now().UTC().Add(-2 * time.Hour) // 2h > 1h soft deadline
	gs.Annotations[agonesv1.GameServerRestartPendingSinceAnnotation] = pendingSinceTime.Format(time.RFC3339)
	gs.Status.State = agonesv1.GameServerStateAllocated

	seedLister(t, factory, gs)
	_, err := fakeClient.AgonesV1().GameServers(testNamespace).Create(
		context.Background(), gs, metav1.CreateOptions{},
	)
	require.NoError(t, err)

	err = c.reconcileRestart(context.Background(), gs)
	require.NoError(t, err)

	updated := lastUpdateGS(t, fakeClient)
	require.NotNil(t, updated, "expected an Update call (advanceAnchor after soft deadline)")

	nextAnnotation, ok := updated.Annotations[agonesv1.GameServerNextRestartAnnotation]
	assert.True(t, ok)

	advancedTime, parseErr := time.Parse(time.RFC3339, nextAnnotation)
	require.NoError(t, parseErr)
	assert.True(t, advancedTime.After(windowOpenedAt),
		"anchor must advance past old window after soft deadline skip")

	_, stillPending := updated.Annotations[agonesv1.GameServerRestartPendingSinceAnnotation]
	assert.False(t, stillPending, "restart-pending-since must be cleared after soft deadline skip")
}

func TestHardDeadlineForce(t *testing.T) {
	hardDeadline := metav1.Duration{Duration: 24 * time.Hour}
	c, fakeClient, factory := newTestRestartController(t)

	gs := newReadyGS(&agonesv1.RestartPolicy{
		Schedule:             everyMinuteCron,
		HardDeadlineDuration: &hardDeadline,
	})

	windowOpenedAt := time.Now().UTC().Add(-2 * time.Minute)
	gs.Annotations[agonesv1.GameServerNextRestartAnnotation] = windowOpenedAt.Format(time.RFC3339)

	pendingSinceTime := time.Now().UTC().Add(-25 * time.Hour) // 25h > 24h hard deadline
	gs.Annotations[agonesv1.GameServerRestartPendingSinceAnnotation] = pendingSinceTime.Format(time.RFC3339)
	gs.Status.State = agonesv1.GameServerStateAllocated

	seedLister(t, factory, gs)
	_, err := fakeClient.AgonesV1().GameServers(testNamespace).Create(
		context.Background(), gs, metav1.CreateOptions{},
	)
	require.NoError(t, err)

	err = c.reconcileRestart(context.Background(), gs)
	require.NoError(t, err)

	updated := lastUpdateGS(t, fakeClient)
	require.NotNil(t, updated, "expected an Update call (hard deadline forced restart)")

	nextAnnotation, ok := updated.Annotations[agonesv1.GameServerNextRestartAnnotation]
	assert.True(t, ok)

	advancedTime, parseErr := time.Parse(time.RFC3339, nextAnnotation)
	require.NoError(t, parseErr)
	assert.True(t, advancedTime.After(windowOpenedAt),
		"anchor must be advanced after hard-deadline forced restart")

	_, stillPending := updated.Annotations[agonesv1.GameServerRestartPendingSinceAnnotation]
	assert.False(t, stillPending, "restart-pending-since must be cleared after hard deadline restart")
}

func TestInvalidCronValidation(t *testing.T) {
	cases := []struct {
		name     string
		schedule string
		wantErr  bool
	}{
		{name: "valid five-field cron", schedule: "0 4 * * *", wantErr: false},
		{name: "valid every-minute cron", schedule: "* * * * *", wantErr: false},
		{name: "invalid: only four fields", schedule: "0 4 * *", wantErr: true},
		{name: "invalid: natural language", schedule: "every day at midnight", wantErr: true},
		{name: "invalid: empty string", schedule: "", wantErr: true},
		{name: "invalid: minute 60 out of range", schedule: "60 4 * * *", wantErr: true},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			gss := &agonesv1.GameServerSpec{
				RestartPolicy: &agonesv1.RestartPolicy{Schedule: tc.schedule},
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{Name: "game-server", Image: "us-docker.pkg.dev/agones-images/examples/simple-game-server:0.35"},
						},
					},
				},
			}

			errs := gss.Validate(noopAPIHooks{}, "", field.NewPath("spec"))

			var rpErrs field.ErrorList
			for _, e := range errs {
				if len(e.Field) >= 13 && e.Field[:13] == "restartPolicy" {
					rpErrs = append(rpErrs, e)
				}
			}

			if tc.wantErr {
				assert.NotEmpty(t, rpErrs,
					"expected restartPolicy validation error for schedule %q", tc.schedule)
				if len(rpErrs) > 0 {
					assert.Equal(t, "restartPolicy.schedule", rpErrs[0].Field)
				}
			} else {
				assert.Empty(t, rpErrs,
					"did NOT expect restartPolicy errors for schedule %q", tc.schedule)
			}
		})
	}
}
func TestNoRestartForTerminalState(t *testing.T) {
	for _, state := range []agonesv1.GameServerState{
		agonesv1.GameServerStateShutdown,
		agonesv1.GameServerStateError,
		agonesv1.GameServerStateUnhealthy,
	} {
		state := state
		t.Run(string(state), func(t *testing.T) {
			c, fakeClient, factory := newTestRestartController(t)

			gs := newReadyGS(&agonesv1.RestartPolicy{Schedule: everyMinuteCron})
			gs.Status.State = state
			// Make the window appear open so the only reason to skip is the terminal state.
			gs.Annotations[agonesv1.GameServerNextRestartAnnotation] =
				time.Now().UTC().Add(-1 * time.Minute).Format(time.RFC3339)

			seedLister(t, factory, gs)
			_, err := fakeClient.AgonesV1().GameServers(testNamespace).Create(
				context.Background(), gs, metav1.CreateOptions{},
			)
			require.NoError(t, err)
			// Clear the Create action so only actions from reconcileRestart are counted.
			fakeClient.ClearActions()

			// Call reconcileRestart directly — the terminal-state guard inside
			// reconcileRestart must return nil before doing any Update.
			require.NoError(t, c.reconcileRestart(context.Background(), gs))

			// No Update calls should have been made.
			for _, a := range fakeClient.Actions() {
				assert.NotEqual(t, "update", a.GetVerb(),
					"must not update a terminal-state GS (%s)", state)
			}
		})
	}
}

func TestNoRestartWithNoPolicy(t *testing.T) {
	c, fakeClient, factory := newTestRestartController(t)

	gs := newReadyGS(nil)
	seedLister(t, factory, gs)
	_, err := fakeClient.AgonesV1().GameServers(testNamespace).Create(
		context.Background(), gs, metav1.CreateOptions{},
	)
	require.NoError(t, err)
	fakeClient.ClearActions()

	require.NoError(t, c.syncGameServer(context.Background(), testNamespace+"/"+testGSName))

	for _, a := range fakeClient.Actions() {
		assert.NotEqual(t, "update", a.GetVerb(),
			"must not touch a GS with no RestartPolicy")
	}
}

func TestRestartDeferredWhenPlayersConnected(t *testing.T) {
	c, fakeClient, factory := newTestRestartController(t)

	gs := newReadyGS(&agonesv1.RestartPolicy{Schedule: everyMinuteCron})
	gs.Annotations[agonesv1.GameServerNextRestartAnnotation] =
		time.Now().UTC().Add(-1 * time.Minute).Format(time.RFC3339)
	gs.Status.State = agonesv1.GameServerStateReady
	gs.Status.Players = &agonesv1.PlayerStatus{Count: 5, Capacity: 10}

	seedLister(t, factory, gs)
	_, err := fakeClient.AgonesV1().GameServers(testNamespace).Create(
		context.Background(), gs, metav1.CreateOptions{},
	)
	require.NoError(t, err)

	require.NoError(t, c.reconcileRestart(context.Background(), gs))

	updated := lastUpdateGS(t, fakeClient)
	require.NotNil(t, updated)
	_, ok := updated.Annotations[agonesv1.GameServerRestartPendingSinceAnnotation]
	assert.True(t, ok, "restart-pending-since must be set when active players block restart")
}
