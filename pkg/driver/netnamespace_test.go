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
	"crypto/rand"
	"fmt"
	"os"
	"path"
	"runtime"
	"testing"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"sigs.k8s.io/dranet/internal/nlwrap"
	"sigs.k8s.io/dranet/pkg/apis"
)

func Test_applyRoutingConfig(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Test requires root privileges.")
	}

	// NewNamed moves the calling thread into the new namespace, so pin the
	// goroutine and put the thread back where we found it afterwards.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	origns, err := netns.Get()
	if err != nil {
		t.Fatalf("unexpected error trying to get namespace: %v", err)
	}
	defer origns.Close()

	rndString := make([]byte, 4)
	if _, err := rand.Read(rndString); err != nil {
		t.Fatalf("fail to generate random name: %v", err)
	}
	nsName := fmt.Sprintf("ns%x", rndString)
	containerNsPath := path.Join("/run/netns", nsName)
	testNS, err := netns.NewNamed(nsName)
	if err != nil {
		t.Fatalf("Failed to create network namespace: %v", err)
	}
	defer netns.DeleteNamed(nsName)
	defer testNS.Close()

	// Switch back to the original namespace
	if err := netns.Set(origns); err != nil {
		t.Fatalf("failed to restore original network namespace: %v", err)
	}

	nhNs, err := nlwrap.NewHandleAt(testNS)
	if err != nil {
		t.Fatalf("fail to open netlink handle: %v", err)
	}
	defer nhNs.Close()

	// A /32 address keeps the gateway off-link, so the universe route installs
	// only after the link route to it: that makes the scope ordering observable.
	ifaceName := "dummy0"
	la := netlink.NewLinkAttrs()
	la.Name = ifaceName
	la.Namespace = netlink.NsFd(int(testNS))
	link := &netlink.Dummy{
		LinkAttrs: la,
	}
	if err := nhNs.LinkAdd(link); err != nil {
		t.Fatalf("Failed to add dummy link %s in ns %s: %v", ifaceName, nsName, err)
	}
	if err := nhNs.LinkSetUp(link); err != nil {
		t.Fatalf("Failed to set up dummy link %s in ns %s: %v", ifaceName, nsName, err)
	}
	addr, err := netlink.ParseAddr("10.10.0.1/32")
	if err != nil {
		t.Fatalf("failed to parse address: %v", err)
	}
	if err := nhNs.AddrAdd(link, addr); err != nil {
		t.Fatalf("failed to add address to %s: %v", ifaceName, err)
	}

	// Universe route first: without applyRoutingConfig sorting by scope, its
	// gateway is unreachable when it is added and the route fails to install.
	routes := []apis.RouteConfig{
		{Destination: "192.168.99.0/24", Gateway: "10.10.0.254", Scope: 0},
		{Destination: "10.10.0.254/32", Scope: 253},
	}
	if err := applyRoutingConfig(containerNsPath, ifaceName, routes, 0); err != nil {
		t.Fatalf("applyRoutingConfig failed: %v", err)
	}

	// applyRoutingConfig sorts a copy, so the shared input keeps its order.
	if routes[0].Destination != "192.168.99.0/24" || routes[1].Destination != "10.10.0.254/32" {
		t.Errorf("applyRoutingConfig reordered its input slice: %+v", routes)
	}

	nsLink, err := nhNs.LinkByName(ifaceName)
	if err != nil {
		t.Fatalf("Failed to get %s after applying routes: %v", ifaceName, err)
	}
	got, err := nhNs.RouteList(nsLink, netlink.FAMILY_V4)
	if err != nil {
		t.Fatalf("failed to list routes: %v", err)
	}
	for _, want := range routes {
		found := false
		for _, r := range got {
			if r.Dst == nil || r.Dst.String() != want.Destination {
				continue
			}
			found = true
			gotGw := ""
			if r.Gw != nil {
				gotGw = r.Gw.String()
			}
			if gotGw != want.Gateway {
				t.Errorf("route %s gateway = %q, want %q", want.Destination, gotGw, want.Gateway)
			}
			if uint8(r.Scope) != want.Scope {
				t.Errorf("route %s scope = %d, want %d", want.Destination, r.Scope, want.Scope)
			}
			break
		}
		if !found {
			t.Errorf("route %s not found in namespace", want.Destination)
		}
	}
}
