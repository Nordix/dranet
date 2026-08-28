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
	"fmt"
	"sync"
	"time"

	"github.com/vishvananda/netlink"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/klog/v2"
	"sigs.k8s.io/dranet/pkg/apis"
)

// healthResendInterval is how often the driver resends the full device
// health snapshot to the kubelet. It must be shorter than the kubelet's
// health lease timeout (HealthCheckTimeout below), otherwise the kubelet
// will treat devices as stale and report them as Unknown between updates.
var healthResendInterval = 10 * time.Second

// healthClientBufferSize buffers health reports per WatchHealthStatus
// subscriber so a burst of updates doesn't block the caller that triggered
// them. If full, notifyHealthClients drops the report instead of blocking;
// the next periodic resend will catch the subscriber up.
const healthClientBufferSize = 4

// deviceHealthEntry is the last known health of a single device.
type deviceHealthEntry struct {
	status  kubeletplugin.HealthStatus
	message string
}

// healthTracker owns the per-device health state and the set of active
// WatchHealthStatus subscribers it is streamed to. mu protects devices,
// clientsMu protects clients.
type healthTracker struct {
	mu        sync.RWMutex
	devices   map[string]*deviceHealthEntry
	clientsMu sync.RWMutex
	clients   []chan kubeletplugin.DeviceHealthReport
	stopCh    chan struct{}
	wg        sync.WaitGroup
}

func newHealthTracker() healthTracker {
	return healthTracker{
		devices: make(map[string]*deviceHealthEntry),
		stopCh:  make(chan struct{}),
	}
}

// sync reconciles the tracked device health entries with the currently
// published set of devices: it adds devices that are seen for the first
// time, updates the status/message of known devices, and drops devices that
// are no longer published. Returns true if anything changed.
func (h *healthTracker) sync(statuses map[string]deviceHealthEntry) bool {
	h.mu.Lock()
	defer h.mu.Unlock()

	changed := false
	for name, want := range statuses {
		entry, ok := h.devices[name]
		if !ok {
			h.devices[name] = &deviceHealthEntry{status: want.status, message: want.message}
			changed = true
			continue
		}
		if entry.status != want.status || entry.message != want.message {
			entry.status = want.status
			entry.message = want.message
			changed = true
		}
	}
	for name := range h.devices {
		if _, ok := statuses[name]; !ok {
			delete(h.devices, name)
			changed = true
		}
	}
	return changed
}

// deviceHealthFromState derives a device's health from its link operational
// state, as published in the apis.AttrState device attribute (see
// pkg/inventory/db.go). Devices whose link is definitively down are
// reported Unhealthy so users can see failing links surfaced in
// pod.status.containerStatuses[].allocatedResourcesStatus. Devices without a
// known state (for example, non-network PCI devices) and devices whose
// operational state can't be determined (netlink.OperUnknown, reported by
// many virtual/passthrough NICs and some drivers that don't implement
// carrier detection) default to Healthy rather than being flagged as a false
// positive.
func deviceHealthFromState(device resourceapi.Device) deviceHealthEntry {
	attr, ok := device.Attributes[apis.AttrState]
	if !ok || attr.StringValue == nil {
		// TODO: devices with no netdev (for example standalone RDMA devices,
		// see discoverStandaloneRDMADevices in pkg/inventory/db.go) have no
		// link operstate to derive health from and are unconditionally
		// reported Healthy. Consider deriving real health for these devices
		// (e.g. RDMA port state) instead of assuming they are always fine.
		return deviceHealthEntry{status: kubeletplugin.HealthStatusHealthy}
	}
	state := *attr.StringValue
	switch state {
	case netlink.LinkOperState(netlink.OperUp).String(),
		netlink.LinkOperState(netlink.OperUnknown).String():
		return deviceHealthEntry{status: kubeletplugin.HealthStatusHealthy}
	}
	return deviceHealthEntry{
		status:  kubeletplugin.HealthStatusUnhealthy,
		message: fmt.Sprintf("link operational state is %q", state),
	}
}

// syncDeviceHealth updates the health tracker from the current set of
// published devices and notifies any active WatchHealthStatus subscribers if
// anything changed. It is called from PublishResources every time the
// device inventory is refreshed.
func (np *NetworkDriver) syncDeviceHealth(devices []resourceapi.Device) {
	statuses := make(map[string]deviceHealthEntry, len(devices))
	for _, d := range devices {
		statuses[d.Name] = deviceHealthFromState(d)
	}
	if np.health.sync(statuses) {
		np.notifyHealthClients()
	}
}

// buildHealthReport snapshots the health of every device this driver
// manages. All devices are published under a single pool named after this
// node (see PublishResources), so PoolName is always np.nodeName.
func (np *NetworkDriver) buildHealthReport() kubeletplugin.DeviceHealthReport {
	h := &np.health
	h.mu.RLock()
	defer h.mu.RUnlock()

	devices := make([]kubeletplugin.DeviceHealth, 0, len(h.devices))
	for name, entry := range h.devices {
		devices = append(devices, kubeletplugin.DeviceHealth{
			PoolName:           np.nodeName,
			DeviceName:         name,
			Health:             entry.status,
			LastUpdated:        time.Now(),
			HealthCheckTimeout: 2 * healthResendInterval,
			Message:            entry.message,
		})
	}
	return kubeletplugin.DeviceHealthReport{Devices: devices}
}

// notifyHealthClients pushes the current health snapshot to every active
// WatchHealthStatus subscriber. Sends are best-effort. A subscriber whose
// channel is full will simply pick up the next periodic resend instead of
// blocking the caller that triggered the change.
func (np *NetworkDriver) notifyHealthClients() {
	report := np.buildHealthReport()

	h := &np.health
	h.clientsMu.RLock()
	defer h.clientsMu.RUnlock()
	for _, ch := range h.clients {
		select {
		case ch <- report:
		default:
		}
	}
}

// healthResendLoop periodically resends the full health snapshot so that the
// kubelet's per-device lease (HealthCheckTimeout) never expires while the
// driver is healthy and simply has nothing new to report. WatchHealthStatus
// deliberately does not resend on its own: a wedged driver should decay to
// Unknown instead of being kept alive artificially.
func (np *NetworkDriver) healthResendLoop(ctx context.Context) {
	defer np.health.wg.Done()

	ticker := time.NewTicker(healthResendInterval)
	defer ticker.Stop()

	for {
		select {
		case <-np.health.stopCh:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			np.notifyHealthClients()
		}
	}
}

// WatchHealthStatus implements kubeletplugin.DRAPlugin. The kubeletplugin
// helper calls it whenever the kubelet subscribes to device health updates
// and takes care of translating the reports produced here into the
// DRAResourceHealth gRPC API that the kubelet supports.
func (np *NetworkDriver) WatchHealthStatus(ctx context.Context, reports chan<- kubeletplugin.DeviceHealthReport) error {
	logger := klog.FromContext(ctx)
	logger.Info("started watching device health updates")
	defer logger.Info("stopped watching device health updates")

	// Buffered so a burst of syncDeviceHealth calls doesn't block on a slow
	// consumer. notifyHealthClients drops reports rather than blocking.
	clientCh := make(chan kubeletplugin.DeviceHealthReport, healthClientBufferSize)
	h := &np.health
	h.clientsMu.Lock()
	h.clients = append(h.clients, clientCh)
	h.clientsMu.Unlock()

	defer func() {
		h.clientsMu.Lock()
		defer h.clientsMu.Unlock()
		for i, ch := range h.clients {
			if ch == clientCh {
				h.clients = append(h.clients[:i], h.clients[i+1:]...)
				break
			}
		}
	}()

	select {
	case <-ctx.Done():
		return nil
	case <-h.stopCh:
		return nil
	case reports <- np.buildHealthReport():
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-h.stopCh:
			return nil
		case report := <-clientCh:
			select {
			case <-ctx.Done():
				return nil
			case <-h.stopCh:
				return nil
			case reports <- report:
			}
		}
	}
}
