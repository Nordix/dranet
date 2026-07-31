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
	"errors"
	"fmt"
	"runtime"

	"github.com/vishvananda/netns"
	"k8s.io/component-helpers/node/util/sysctl"

	"sigs.k8s.io/dranet/pkg/apis"
)

// sysctlProvider is overridden in tests to exercise sysctl failure paths.
var sysctlProvider = sysctl.New

// hasARPConfig reports whether the interface config asks for any ARP setting.
func hasARPConfig(interfaceConfig apis.InterfaceConfig) bool {
	return interfaceConfig.ARPIgnore != nil || interfaceConfig.ARPAnnounce != nil
}

func applyInterfaceARPWithSysctl(sysctlInterface sysctl.Interface, ifName string, interfaceConfig apis.InterfaceConfig) error {
	var errorList []error
	set := func(setting string, value int32) {
		name := fmt.Sprintf("net/ipv4/conf/%s/%s", ifName, setting)
		if err := sysctlInterface.SetSysctl(name, int(value)); err != nil {
			errorList = append(errorList, fmt.Errorf("failed to set %s: %w", name, err))
		}
	}

	if interfaceConfig.ARPIgnore != nil {
		set("arp_ignore", *interfaceConfig.ARPIgnore)
	}
	if interfaceConfig.ARPAnnounce != nil {
		set("arp_announce", *interfaceConfig.ARPAnnounce)
	}
	return errors.Join(errorList...)
}

// applyInterfaceARPConfig sets the requested per-interface ARP sysctls inside the
// Pod network namespace. These live under /proc/sys, so unlike the rest of the
// interface configuration they cannot be set through a netlink handle and
// require joining the namespace.
func applyInterfaceARPConfig(containerNsPath string, ifName string, interfaceConfig apis.InterfaceConfig) error {
	if !hasARPConfig(interfaceConfig) {
		return nil
	}

	result := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		unlockThread := true
		defer func() {
			if unlockThread {
				runtime.UnlockOSThread()
			}
		}()

		originalNs, err := netns.Get()
		if err != nil {
			result <- fmt.Errorf("failed to get current network namespace: %w", err)
			return
		}
		defer originalNs.Close()

		containerNs, err := netns.GetFromPath(containerNsPath)
		if err != nil {
			result <- fmt.Errorf("could not get network namespace from path %s: %w", containerNsPath, err)
			return
		}
		defer containerNs.Close()

		if err := netns.Set(containerNs); err != nil {
			result <- fmt.Errorf("failed to join network namespace %s: %w", containerNsPath, err)
			return
		}

		applyErr := applyInterfaceARPWithSysctl(sysctlProvider(), ifName, interfaceConfig)
		if err := netns.Set(originalNs); err != nil {
			// Keep this thread locked so the runtime destroys it instead of
			// reusing it in the wrong network namespace.
			unlockThread = false
			result <- errors.Join(applyErr, fmt.Errorf("failed to restore network namespace: %w", err))
			return
		}
		result <- applyErr
	}()
	return <-result
}
