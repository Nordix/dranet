/*
Copyright The Kubernetes Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package driver

import (
	"context"
	"sync"
	"testing"
	"time"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/dranet/pkg/apis"
)

const (
	healthTestDriverName = "test.driver"
	healthTestNodeName   = "test-node"
)

func newHealthTestDriver() *NetworkDriver {
	return &NetworkDriver{
		driverName: healthTestDriverName,
		nodeName:   healthTestNodeName,
		health:     newHealthTracker(),
	}
}

func deviceWithState(name, state string) resourceapi.Device {
	d := resourceapi.Device{Name: name, Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{}}
	if state != "" {
		d.Attributes[apis.AttrState] = resourceapi.DeviceAttribute{StringValue: ptr.To(state)}
	}
	return d
}

func TestDeviceHealthFromState(t *testing.T) {
	testCases := []struct {
		name       string
		device     resourceapi.Device
		wantStatus kubeletplugin.HealthStatus
		wantEmpty  bool
	}{
		{
			name:       "up is healthy",
			device:     deviceWithState("dev0", "up"),
			wantStatus: kubeletplugin.HealthStatusHealthy,
			wantEmpty:  true,
		},
		{
			name:       "down is unhealthy",
			device:     deviceWithState("dev0", "down"),
			wantStatus: kubeletplugin.HealthStatusUnhealthy,
		},
		{
			name:       "lower layer down is unhealthy",
			device:     deviceWithState("dev0", "lowerlayerdown"),
			wantStatus: kubeletplugin.HealthStatusUnhealthy,
		},
		{
			name:       "unknown is healthy",
			device:     deviceWithState("dev0", "unknown"),
			wantStatus: kubeletplugin.HealthStatusHealthy,
			wantEmpty:  true,
		},
		{
			name:       "missing state defaults to healthy",
			device:     deviceWithState("dev0", ""),
			wantStatus: kubeletplugin.HealthStatusHealthy,
			wantEmpty:  true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := deviceHealthFromState(tc.device)
			if got.status != tc.wantStatus {
				t.Errorf("status = %v, want %v", got.status, tc.wantStatus)
			}
			if tc.wantEmpty && got.message != "" {
				t.Errorf("message = %q, want empty", got.message)
			}
			if !tc.wantEmpty && got.message == "" {
				t.Errorf("message = empty, want non-empty")
			}
		})
	}
}

func TestSyncDeviceHealth(t *testing.T) {
	np := newHealthTestDriver()
	clientCh := make(chan kubeletplugin.DeviceHealthReport, 1)
	np.health.clientsMu.Lock()
	np.health.clients = append(np.health.clients, clientCh)
	np.health.clientsMu.Unlock()

	// First sync: two new devices, one up and one down. Both are new so a
	// notification is expected.
	np.syncDeviceHealth([]resourceapi.Device{
		deviceWithState("dev0", "up"),
		deviceWithState("dev1", "down"),
	})

	select {
	case report := <-clientCh:
		byName := map[string]kubeletplugin.DeviceHealth{}
		for _, d := range report.Devices {
			byName[d.DeviceName] = d
		}
		if got := byName["dev0"].Health; got != kubeletplugin.HealthStatusHealthy {
			t.Errorf("dev0 health = %v, want Healthy", got)
		}
		if got := byName["dev1"].Health; got != kubeletplugin.HealthStatusUnhealthy {
			t.Errorf("dev1 health = %v, want Unhealthy", got)
		}
		if byName["dev0"].PoolName != healthTestNodeName {
			t.Errorf("PoolName = %q, want %q", byName["dev0"].PoolName, healthTestNodeName)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected a health report after the first sync")
	}

	// Second sync with identical state: no change, no notification.
	np.syncDeviceHealth([]resourceapi.Device{
		deviceWithState("dev0", "up"),
		deviceWithState("dev1", "down"),
	})
	select {
	case <-clientCh:
		t.Fatal("did not expect a health report for a no-op sync")
	default:
	}

	// Third sync: dev1 recovers and dev0 disappears. Expect a notification
	// and dev0 removed from the report.
	np.syncDeviceHealth([]resourceapi.Device{
		deviceWithState("dev1", "up"),
	})
	select {
	case report := <-clientCh:
		if len(report.Devices) != 1 {
			t.Fatalf("len(report.Devices) = %d, want 1", len(report.Devices))
		}
		if report.Devices[0].DeviceName != "dev1" || report.Devices[0].Health != kubeletplugin.HealthStatusHealthy {
			t.Errorf("unexpected device health: %+v", report.Devices[0])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected a health report after devices changed")
	}
}

func TestWatchHealthStatus(t *testing.T) {
	np := newHealthTestDriver()
	np.syncDeviceHealth([]resourceapi.Device{deviceWithState("dev0", "up")})

	ctx, cancel := context.WithCancel(context.Background())
	reports := make(chan kubeletplugin.DeviceHealthReport)

	var wg sync.WaitGroup
	wg.Add(1)
	var watchErr error
	go func() {
		defer wg.Done()
		watchErr = np.WatchHealthStatus(ctx, reports)
	}()

	// The first report sent must be a full snapshot of the current state.
	select {
	case report := <-reports:
		if len(report.Devices) != 1 || report.Devices[0].Health != kubeletplugin.HealthStatusHealthy {
			t.Fatalf("unexpected initial report: %+v", report)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the initial health report")
	}

	// A subsequent health change must be streamed too.
	go np.syncDeviceHealth([]resourceapi.Device{deviceWithState("dev0", "down")})

	select {
	case report := <-reports:
		if len(report.Devices) != 1 || report.Devices[0].Health != kubeletplugin.HealthStatusUnhealthy {
			t.Fatalf("unexpected updated report: %+v", report)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the updated health report")
	}

	np.health.clientsMu.RLock()
	numClients := len(np.health.clients)
	np.health.clientsMu.RUnlock()
	if numClients != 1 {
		t.Fatalf("numClients = %d, want 1 while WatchHealthStatus is running", numClients)
	}

	cancel()
	wg.Wait()
	if watchErr != nil {
		t.Fatalf("WatchHealthStatus returned error: %v", watchErr)
	}

	np.health.clientsMu.RLock()
	numClients = len(np.health.clients)
	np.health.clientsMu.RUnlock()
	if numClients != 0 {
		t.Fatalf("numClients = %d, want 0 after WatchHealthStatus returns", numClients)
	}
}

func TestHealthResendLoop(t *testing.T) {
	np := newHealthTestDriver()
	np.syncDeviceHealth([]resourceapi.Device{deviceWithState("dev0", "up")})
	healthResendInterval = 20 * time.Millisecond
	defer func() { healthResendInterval = 10 * time.Second }()

	clientCh := make(chan kubeletplugin.DeviceHealthReport, 4)
	np.health.clientsMu.Lock()
	np.health.clients = append(np.health.clients, clientCh)
	np.health.clientsMu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	np.health.wg.Add(1)
	go np.healthResendLoop(ctx)

	select {
	case <-clientCh:
	case <-time.After(2 * time.Second):
		t.Fatal("expected a periodic health resend")
	}

	cancel()
	np.health.wg.Wait()
}
