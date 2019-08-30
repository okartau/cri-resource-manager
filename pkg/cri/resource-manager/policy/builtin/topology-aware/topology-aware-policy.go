// Copyright 2019 Intel Corporation. All Rights Reserved.
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

package topologyaware

import (
	resapi "k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpuset"

	"github.com/intel/cri-resource-manager/pkg/cri/resource-manager/cache"
	policyapi "github.com/intel/cri-resource-manager/pkg/cri/resource-manager/policy"
	system "github.com/intel/cri-resource-manager/pkg/sysfs"
	"strings"
)

const (
	// PolicyName is the symbol used to pull us in as a builtin policy.
	PolicyName = "topology-aware"
	// PolicyDescription is a short description of this policy.
	PolicyDescription = "A policy for HW-topology aware workload placement."
)

// allocations is our cache.Cachable for saving resource allocations in the cache.
type allocations struct {
	policy *policy
	CPU    map[string]CPUGrant
}

// policy is our runtime state for the topology aware policy.
type policy struct {
	options     policyapi.Options // options we were created or reconfigured with
	cache       cache.Cache       // pod/container cache
	sys         *system.System    // system/HW topology info
	allowed     cpuset.CPUSet     // bounding set of CPUs we're allowed to use
	reserved    cpuset.CPUSet     // system-/kube-reserved CPUs
	reserveCnt  int               // number of CPUs to reserve if given as resource.Quantity
	isolated    cpuset.CPUSet     // (our allowed set of) isolated CPUs
	nodes       map[string]Node   // pool nodes by name
	pools       []Node            // pre-populated node slice for scoring, etc...
	root        Node              // root of our pool/partition tree
	nodeCnt     int               // number of pools
	depth       int               // tree depth
	allocations allocations       // container pool assignments

}

// Make sure policy implements the policy.Backend interface.
var _ policyapi.Backend = &policy{}

// CreateTopologyAwarePolicy creates a new policy instance.
func CreateTopologyAwarePolicy(opts *policyapi.Options) policyapi.Backend {
	p := &policy{options: *opts}

	p.nodes = make(map[string]Node)
	p.allocations = allocations{policy: p, CPU: make(map[string]CPUGrant, 32)}

	if err := p.discoverSystemTopology(); err != nil {
		log.Fatal("failed to create topology-aware policy: %v", err)
	}

	if err := p.checkConstraints(); err != nil {
		log.Fatal("failed to create topology-aware policy: %v", err)
	}

	if err := p.buildPoolsByTopology(); err != nil {
		log.Fatal("failed to create topology-aware policy: %v", err)
	}

	p.root.Dump("<pre-start>")

	return p
}

// Name returns the name of this policy.
func (p *policy) Name() string {
	return PolicyName
}

// Description returns the description for this policy.
func (p *policy) Description() string {
	return PolicyDescription
}

// Start prepares this policy for accepting allocation/release requests.
func (p *policy) Start(cch cache.Cache) error {
	p.cache = cch

	if err := p.restoreCache(); err != nil {
		return policyError("failed to start: %v", err)
	}

	p.root.Dump("<post-start>")

	return nil
}

// AllocateResources is a resource allocation request for this policy.
func (p *policy) AllocateResources(container cache.Container) error {
	log.Debug("allocating resources for %s...", container.GetCacheID())

	grant, err := p.allocatePool(container)
	if err != nil {
		return policyError("failed to allocate resources for %s: %v",
			container.GetCacheID(), err)
	}

	if err := p.applyGrant(grant); err != nil {
		if _, _, err = p.releasePool(container); err != nil {
			log.Warn("failed to undo/release unapplicable grant %s: %v",
				grant.String(), err)
			return policyError("failed to undo/release unapplicable grant %s: %v",
				grant.String(), err)
		}
	}

	if err := p.updateSharedAllocations(grant); err != nil {
		log.Warn("failed to update shared allocations affected by %s: %v",
			container.GetCacheID(), err)
	}

	p.root.Dump("<post-alloc>")

	return nil
}

// ReleaseResources is a resource release request for this policy.
func (p *policy) ReleaseResources(container cache.Container) error {
	log.Debug("releasing resources of %s...", container.GetCacheID())

	grant, found, err := p.releasePool(container)
	if err != nil {
		return policyError("failed to release resources of %s: %v",
			container.GetCacheID(), err)
	}

	if found {
		if err = p.updateSharedAllocations(grant); err != nil {
			log.Warn("failed to update shared allocations affected by %s: %v",
				container.GetCacheID(), err)
		}
	}

	p.root.Dump("<post-release>")

	return nil
}

// UpdateResources is a resource allocation update request for this policy.
func (p *policy) UpdateResources(c cache.Container) error {
	log.Debug("(not) updating container %s...", c.GetCacheID())
	return nil
}

// ExportResourceData provides resource data to export for the container.
func (p *policy) ExportResourceData(c cache.Container, syntax policyapi.DataSyntax) []byte {
	return nil
}

func (p *policy) PostStart(cch cache.Container) error {
	log.Debug("post start container...")
	return nil
}

// SetConfig sets the policy backend configuration.
func (p *policy) SetConfig(rawConf string) error {
	if rawConf = strings.TrimSpace(rawConf); rawConf == "" {
		return nil
	}

	conf, err := parseConfig([]byte(rawConf))
	if err != nil {
		return err
	}

	if opt.PinCPU != conf.PinCPU {
		opt.PinCPU = conf.PinCPU
		log.Info("pin containers to CPUs: %v", opt.PinCPU)
	}

	if opt.PinMem != conf.PinMem {
		opt.PinMem = conf.PinMem
		log.Info("pin containers to memory: %v", opt.PinMem)
	}

	if opt.PreferIsolated != conf.PreferIsolated {
		opt.PreferIsolated = conf.PreferIsolated
		log.Info("use kernel-isolated CPUs for exclusive allocation: %v",
			opt.PreferIsolated)
	}

	if opt.PreferShared != conf.PreferShared {
		opt.PreferShared = conf.PreferShared
		log.Info("prefer shared CPU allocation: %v", opt.PreferShared)
	}

	// TODO: We probably should release and reallocate resources for all containers
	//   to honor the latest configuration. Depending on the changes that might be
	//   disruptive to some containers, so whether we do so or not should probably
	//   be part of the configuration as well.

	p.saveConfig()

	return nil
}

// Discover system topology.
func (p *policy) discoverSystemTopology() error {
	var err error

	log.Info("discovering system topology...")
	if p.sys, err = system.DiscoverSystem(); err != nil {
		return policyError("failed to discover system topology: %v", err)
	}

	return nil
}

// Check the constraints passed to us.
func (p *policy) checkConstraints() error {
	if c, ok := p.options.Available[policyapi.DomainCPU]; ok {
		p.allowed = c.(cpuset.CPUSet)
	} else {
		// default to all online cpus
		p.allowed = p.sys.CPUSet().Difference(p.sys.Offlined())
	}

	p.isolated = p.sys.Isolated().Intersection(p.allowed)

	c, ok := p.options.Reserved[policyapi.DomainCPU]
	if !ok {
		return policyError("cannot start without CPU reservation")
	}

	switch c.(type) {
	case cpuset.CPUSet:
		p.reserved = c.(cpuset.CPUSet)
		// check that all reserved CPUs are in the allowed set
		if !p.reserved.Difference(p.allowed).IsEmpty() {
			return policyError("invalid reserved cpuset %s, some CPUs (%s) are not "+
				"part of the online allowed cpuset (%s)", p.reserved.String(),
				p.reserved.Difference(p.allowed).String(), p.allowed.String())
		}
		// check that none of the reserved CPUs are isolated
		if !p.reserved.Intersection(p.isolated).IsEmpty() {
			return policyError("invalid reserved cpuset %s, some CPUs (%s) are also isolated",
				p.reserved.Intersection(p.isolated).String())
		}

	case resapi.Quantity:
		qty := c.(resapi.Quantity)
		p.reserveCnt = (int(qty.MilliValue()) + 999) / 1000
	}

	return nil
}

func (p *policy) restoreCache() error {
	if !p.restoreConfig() {
		log.Warn("no saved configuration found in cache...")
		p.saveConfig()
	}

	if !p.restoreAllocations() {
		log.Warn("no allocations found in cache...")
		p.saveAllocations()
	} else {
		p.allocations.Dump(log.Info, "restored ")
	}

	return nil
}

//
// Automatically register us as a policy implementation.
//

// Implementation is the implementation we register with the policy module.
type Implementation func(*policyapi.Options) policyapi.Backend

// Name returns the name of this policy implementation.
func (Implementation) Name() string {
	return PolicyName
}

// Description returns the desccription of this policy implementation.
func (Implementation) Description() string {
	return PolicyDescription
}

// CreateFn returns the functions used to instantiate this policy.
func (i Implementation) CreateFn() policyapi.CreateFn {
	return policyapi.CreateFn(i)
}

var _ policyapi.Implementation = Implementation(nil)

func init() {
	policyapi.Register(Implementation(CreateTopologyAwarePolicy))
}
