/*
 * Copyright (c) 2022, NVIDIA CORPORATION.  All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY Type, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package rm

import (
	"fmt"
	"sort"

	"github.com/NVIDIA/go-gpuallocator/gpuallocator"
	config "github.com/NVIDIA/k8s-device-plugin/api/config/v1"
)

var alignedAllocationPolicy = gpuallocator.NewBestEffortPolicy()

// GetPreferredAllocation runs an allocation algorithm over the inputs.
// The algorithm chosen is based both on the incoming set of available devices and various config settings.
func (r *resourceManager) GetPreferredAllocation(available, required []string, size int) ([]string, error) {
	// If all of the available devices are full GPUs without replicas.  then
	// calculate an aligned allocation of across those devices.
	if !r.Devices().ContainsMigDevices() && !AnnotatedIDs(available).AnyHasAnnotations() {
		return r.alignedAllocation(available, required, size)
	}

	// Otherwise, if the time-slicing policy in place is "packed", run that algorithm.
	if r.config.Sharing.TimeSlicing.Strategy == config.TimeSlicingStrategyPacked {
		return r.packedAllocation(available, required, size)
	}

	// Otherwise, if the time-slicing policy in place is "distributed", run that algorithm.
	if r.config.Sharing.TimeSlicing.Strategy == config.TimeSlicingStrategyDistributed {
		return r.distributedAllocation(available, required, size)
	}

	// Otherwise, error out.
	return nil, fmt.Errorf("no valid allocation policy selected")
}

// alignedAllocation shells out to the alignedAllocationPolicy that is set in
// order to calculate the preferred allocation.
func (r *resourceManager) alignedAllocation(available, required []string, size int) ([]string, error) {
	var devices []string

	availableDevices, err := gpuallocator.NewDevicesFrom(available)
	if err != nil {
		return nil, fmt.Errorf("unable to retrieve list of available devices: %v", err)
	}

	requiredDevices, err := gpuallocator.NewDevicesFrom(required)
	if err != nil {
		return nil, fmt.Errorf("unable to retrieve list of required devices: %v", err)
	}

	allocatedDevices := alignedAllocationPolicy.Allocate(availableDevices, requiredDevices, size)

	for _, device := range allocatedDevices {
		devices = append(devices, device.UUID)
	}

	return devices, nil
}

// packedAllocation returns a list of sorted devices, being sure to include any
// required ones at the front. Sorting them ensures that devices from the same
// GPU (in the case of sharing) are chosen first before moving on to the next
// one (i.e we follow a packed sharing strategy rather than a distributed one).
func (r *resourceManager) packedAllocation(available, required []string, size int) ([]string, error) {
	candidates := r.devices.Subset(available).Difference(r.devices.Subset(required)).GetIDs()
	sort.Strings(candidates)

	devices := append(required, candidates...)
	if len(devices) < size {
		return nil, fmt.Errorf("not enough available devices to satisfy allocation")
	}

	return devices[:size], nil
}

// distributedAllocation returns a list of devices such that any replicated
// devices are distributed across all replicated GPUs equally. It takes into
// account already allocated replicas to ensure a proper balance across them.
func (r *resourceManager) distributedAllocation(available, required []string, size int) ([]string, error) {
	// Get the set of candidate devices as the difference between available and required.
	candidates := r.devices.Subset(available).Difference(r.devices.Subset(required)).GetIDs()
	needed := size - len(required)

	if len(candidates) < needed {
		return nil, fmt.Errorf("not enough available devices to satisfy allocation")
	}

	// For each candidate device, build a mapping of (stripped) device ID to
	// total / available replicas for that device.
	replicas := make(map[string]*struct{ total, available int })
	for _, c := range candidates {
		id := AnnotatedID(c).GetID()
		replicas[id] = &struct{ total, available int }{}
		replicas[id].available++
	}
	for d := range r.devices {
		id := AnnotatedID(d).GetID()
		if _, exists := replicas[id]; !exists {
			continue
		}
		replicas[id].total++
	}

	// Grab the set of 'needed' devices one-by-one from the candidates list.
	// Before selecting each candidate, first sort the candidate list using the
	// replicas map above. After sorting, the first element in the list will
	// contain the device with the least difference between total and available
	// replications (based on what's already been allocated). Add this device
	// to the list of devices to allocate, remove it from the candidate list,
	// down its available count in the replicas map, and repeat.
	var devices []string
	for i := 0; i < needed; i++ {
		sort.Slice(candidates, func(i, j int) bool {
			iid := AnnotatedID(candidates[i]).GetID()
			jid := AnnotatedID(candidates[j]).GetID()
			idiff := replicas[iid].total - replicas[iid].available
			jdiff := replicas[jid].total - replicas[jid].available
			return idiff < jdiff
		})
		id := AnnotatedID(candidates[0]).GetID()
		replicas[id].available--
		devices = append(devices, candidates[0])
		candidates = candidates[1:]
	}

	// Add the set of required devices to this list and return it.
	devices = append(required, devices...)

	return devices, nil
}
