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
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	sysctltesting "k8s.io/component-helpers/node/util/sysctl/testing"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/dranet/pkg/apis"
)

// failingSetSysctl fails writes to a single setting and delegates the rest.
type failingSetSysctl struct {
	*sysctltesting.Fake
	setting string
}

func (f *failingSetSysctl) SetSysctl(setting string, value int) error {
	if setting == f.setting {
		return errors.New("test set failure")
	}
	return f.Fake.SetSysctl(setting, value)
}

func TestHasARPConfig(t *testing.T) {
	tests := []struct {
		name            string
		interfaceConfig apis.InterfaceConfig
		want            bool
	}{
		{
			name:            "empty",
			interfaceConfig: apis.InterfaceConfig{Name: "eth0"},
			want:            false,
		},
		{
			name:            "arp ignore only",
			interfaceConfig: apis.InterfaceConfig{ARPIgnore: ptr.To[int32](1)},
			want:            true,
		},
		{
			name:            "arp announce only",
			interfaceConfig: apis.InterfaceConfig{ARPAnnounce: ptr.To[int32](2)},
			want:            true,
		},
		{
			name:            "zero values are still requested",
			interfaceConfig: apis.InterfaceConfig{ARPIgnore: ptr.To[int32](0), ARPAnnounce: ptr.To[int32](0)},
			want:            true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasARPConfig(tt.interfaceConfig); got != tt.want {
				t.Errorf("hasARPConfig() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestApplyInterfaceARPWithSysctl(t *testing.T) {
	tests := []struct {
		name            string
		interfaceConfig apis.InterfaceConfig
		want            map[string]int
	}{
		{
			name: "both settings",
			interfaceConfig: apis.InterfaceConfig{
				ARPIgnore:   ptr.To[int32](1),
				ARPAnnounce: ptr.To[int32](2),
			},
			want: map[string]int{
				"net/ipv4/conf/rdma0/arp_ignore":   1,
				"net/ipv4/conf/rdma0/arp_announce": 2,
			},
		},
		{
			name:            "only arp_ignore",
			interfaceConfig: apis.InterfaceConfig{ARPIgnore: ptr.To[int32](1)},
			want:            map[string]int{"net/ipv4/conf/rdma0/arp_ignore": 1},
		},
		{
			name:            "explicit zero is written",
			interfaceConfig: apis.InterfaceConfig{ARPAnnounce: ptr.To[int32](0)},
			want:            map[string]int{"net/ipv4/conf/rdma0/arp_announce": 0},
		},
		{
			name:            "nothing requested",
			interfaceConfig: apis.InterfaceConfig{Name: "rdma0"},
			want:            map[string]int{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sysctls := sysctltesting.NewFake()
			if err := applyInterfaceARPWithSysctl(sysctls, "rdma0", tt.interfaceConfig); err != nil {
				t.Fatalf("applyInterfaceARPWithSysctl() error: %v", err)
			}
			if diff := cmp.Diff(tt.want, sysctls.Settings); diff != "" {
				t.Errorf("applyInterfaceARPWithSysctl() settings mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestApplyInterfaceARPWithSysctlReturnsSetErrors(t *testing.T) {
	sysctls := &failingSetSysctl{
		Fake:    sysctltesting.NewFake(),
		setting: "net/ipv4/conf/rdma0/arp_ignore",
	}
	interfaceConfig := apis.InterfaceConfig{
		ARPIgnore:   ptr.To[int32](1),
		ARPAnnounce: ptr.To[int32](2),
	}

	err := applyInterfaceARPWithSysctl(sysctls, "rdma0", interfaceConfig)
	if err == nil || !strings.Contains(err.Error(), sysctls.setting) {
		t.Fatalf("applyInterfaceARPWithSysctl() error = %v, want error naming %s", err, sysctls.setting)
	}
	// A failed setting must not stop the remaining ones from being applied.
	if len(sysctls.Settings) != 1 {
		t.Errorf("applyInterfaceARPWithSysctl() applied %d settings, want 1", len(sysctls.Settings))
	}
}

func TestApplyInterfaceARPConfigNoConfigDoesNotEnterNamespace(t *testing.T) {
	// An empty config must return before touching the namespace path, so a
	// path that cannot be opened is still not an error.
	if err := applyInterfaceARPConfig("/nonexistent/netns", "rdma0", apis.InterfaceConfig{Name: "rdma0"}); err != nil {
		t.Fatalf("applyInterfaceARPConfig() error: %v", err)
	}
}
