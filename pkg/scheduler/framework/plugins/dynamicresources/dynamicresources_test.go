/*
Copyright 2022 The Kubernetes Authors.

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

package dynamicresources

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1alpha3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	cgotesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/feature"
	"k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	"k8s.io/kubernetes/pkg/scheduler/util/assumecache"
	"k8s.io/kubernetes/test/utils/ktesting"
	"k8s.io/utils/ptr"
)

var (
	podKind = v1.SchemeGroupVersion.WithKind("Pod")

	nodeName      = "worker"
	node2Name     = "worker-2"
	node3Name     = "worker-3"
	controller    = "some-driver"
	driver        = controller
	podName       = "my-pod"
	podUID        = "1234"
	resourceName  = "my-resource"
	resourceName2 = resourceName + "-2"
	claimName     = podName + "-" + resourceName
	claimName2    = podName + "-" + resourceName + "-2"
	className     = "my-resource-class"
	namespace     = "default"
	attrName      = resourceapi.QualifiedName("healthy") // device attribute only available on non-default node

	deviceClass = &resourceapi.DeviceClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: className,
		},
	}

	podWithClaimName = st.MakePod().Name(podName).Namespace(namespace).
				UID(podUID).
				PodResourceClaims(v1.PodResourceClaim{Name: resourceName, ResourceClaimName: &claimName}).
				Obj()
	podWithClaimTemplate = st.MakePod().Name(podName).Namespace(namespace).
				UID(podUID).
				PodResourceClaims(v1.PodResourceClaim{Name: resourceName, ResourceClaimTemplateName: &claimName}).
				Obj()
	podWithClaimTemplateInStatus = func() *v1.Pod {
		pod := podWithClaimTemplate.DeepCopy()
		pod.Status.ResourceClaimStatuses = []v1.PodResourceClaimStatus{
			{
				Name:              pod.Spec.ResourceClaims[0].Name,
				ResourceClaimName: &claimName,
			},
		}
		return pod
	}()
	podWithTwoClaimNames = st.MakePod().Name(podName).Namespace(namespace).
				UID(podUID).
				PodResourceClaims(v1.PodResourceClaim{Name: resourceName, ResourceClaimName: &claimName}).
				PodResourceClaims(v1.PodResourceClaim{Name: resourceName2, ResourceClaimName: &claimName2}).
				Obj()

	// Node with "instance-1" device and no device attributes.
	workerNode      = &st.MakeNode().Name(nodeName).Label("kubernetes.io/hostname", nodeName).Node
	workerNodeSlice = st.MakeResourceSlice(nodeName, driver).Device("instance-1", nil).Obj()

	// Node with same device, but now with a "healthy" boolean attribute.
	workerNode2      = &st.MakeNode().Name(node2Name).Label("kubernetes.io/hostname", node2Name).Node
	workerNode2Slice = st.MakeResourceSlice(node2Name, driver).Device("instance-1", map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{attrName: {BoolValue: ptr.To(true)}}).Obj()

	// Yet another node, same as the second one.
	workerNode3      = &st.MakeNode().Name(node3Name).Label("kubernetes.io/hostname", node3Name).Node
	workerNode3Slice = st.MakeResourceSlice(node3Name, driver).Device("instance-1", map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{attrName: {BoolValue: ptr.To(true)}}).Obj()

	brokenSelector = resourceapi.DeviceSelector{
		CEL: &resourceapi.CELDeviceSelector{
			// Not set for workerNode.
			Expression: fmt.Sprintf(`device.attributes["%s"].%s`, driver, attrName),
		},
	}

	claim = st.MakeResourceClaim(controller).
		Name(claimName).
		Namespace(namespace).
		Request(className).
		Obj()
	pendingClaim = st.FromResourceClaim(claim).
			OwnerReference(podName, podUID, podKind).
			Obj()
	pendingClaim2 = st.FromResourceClaim(pendingClaim).
			Name(claimName2).
			Obj()
	allocationResult = &resourceapi.AllocationResult{
		Controller: controller,
		Devices: resourceapi.DeviceAllocationResult{
			Results: []resourceapi.DeviceRequestAllocationResult{{
				Driver:  driver,
				Pool:    nodeName,
				Device:  "instance-1",
				Request: "req-1",
			}},
		},
		NodeSelector: func() *v1.NodeSelector {
			// Label selector...
			nodeSelector := st.MakeNodeSelector().In("metadata.name", []string{nodeName}).Obj()
			// ... but we need a field selector, so let's swap.
			nodeSelector.NodeSelectorTerms[0].MatchExpressions, nodeSelector.NodeSelectorTerms[0].MatchFields = nodeSelector.NodeSelectorTerms[0].MatchFields, nodeSelector.NodeSelectorTerms[0].MatchExpressions
			return nodeSelector
		}(),
	}
	deallocatingClaim = st.FromResourceClaim(pendingClaim).
				Allocation(allocationResult).
				DeallocationRequested(true).
				Obj()
	inUseClaim = st.FromResourceClaim(pendingClaim).
			Allocation(allocationResult).
			ReservedForPod(podName, types.UID(podUID)).
			Obj()
	structuredInUseClaim = st.FromResourceClaim(inUseClaim).
				Structured().
				Obj()
	allocatedClaim = st.FromResourceClaim(pendingClaim).
			Allocation(allocationResult).
			Obj()

	allocatedClaimWithWrongTopology = st.FromResourceClaim(allocatedClaim).
					Allocation(&resourceapi.AllocationResult{Controller: controller, NodeSelector: st.MakeNodeSelector().In("no-such-label", []string{"no-such-value"}).Obj()}).
					Obj()
	allocatedClaimWithGoodTopology = st.FromResourceClaim(allocatedClaim).
					Allocation(&resourceapi.AllocationResult{Controller: controller, NodeSelector: st.MakeNodeSelector().In("kubernetes.io/hostname", []string{nodeName}).Obj()}).
					Obj()
	otherClaim = st.MakeResourceClaim(controller).
			Name("not-my-claim").
			Namespace(namespace).
			Request(className).
			Obj()
	otherAllocatedClaim = st.FromResourceClaim(otherClaim).
				Allocation(allocationResult).
				Obj()

	scheduling = st.MakePodSchedulingContexts().Name(podName).Namespace(namespace).
			OwnerReference(podName, podUID, podKind).
			Obj()
	schedulingPotential = st.FromPodSchedulingContexts(scheduling).
				PotentialNodes(workerNode.Name).
				Obj()
	schedulingSelectedPotential = st.FromPodSchedulingContexts(schedulingPotential).
					SelectedNode(workerNode.Name).
					Obj()
	schedulingInfo = st.FromPodSchedulingContexts(schedulingPotential).
			ResourceClaims(resourceapi.ResourceClaimSchedulingStatus{Name: resourceName},
			resourceapi.ResourceClaimSchedulingStatus{Name: resourceName2}).
		Obj()
)

func reserve(claim *resourceapi.ResourceClaim, pod *v1.Pod) *resourceapi.ResourceClaim {
	return st.FromResourceClaim(claim).
		ReservedForPod(pod.Name, types.UID(pod.UID)).
		Obj()
}

func structuredClaim(claim *resourceapi.ResourceClaim) *resourceapi.ResourceClaim {
	return st.FromResourceClaim(claim).
		Structured().
		Obj()
}

func breakCELInClaim(claim *resourceapi.ResourceClaim) *resourceapi.ResourceClaim {
	claim = claim.DeepCopy()
	for i := range claim.Spec.Devices.Requests {
		for e := range claim.Spec.Devices.Requests[i].Selectors {
			claim.Spec.Devices.Requests[i].Selectors[e] = brokenSelector
		}
		if len(claim.Spec.Devices.Requests[i].Selectors) == 0 {
			claim.Spec.Devices.Requests[i].Selectors = []resourceapi.DeviceSelector{brokenSelector}
		}
	}
	return claim
}

func breakCELInClass(class *resourceapi.DeviceClass) *resourceapi.DeviceClass {
	class = class.DeepCopy()
	for i := range class.Spec.Selectors {
		class.Spec.Selectors[i] = brokenSelector
	}
	if len(class.Spec.Selectors) == 0 {
		class.Spec.Selectors = []resourceapi.DeviceSelector{brokenSelector}
	}

	return class
}

// result defines the expected outcome of some operation. It covers
// operation's status and the state of the world (= objects).
type result struct {
	status *framework.Status
	// changes contains a mapping of name to an update function for
	// the corresponding object. These functions apply exactly the expected
	// changes to a copy of the object as it existed before the operation.
	changes change

	// added contains objects created by the operation.
	added []metav1.Object

	// removed contains objects deleted by the operation.
	removed []metav1.Object

	// assumedClaim is the one claim which is expected to be assumed,
	// nil if none.
	assumedClaim *resourceapi.ResourceClaim

	// inFlightClaim is the one claim which is expected to be tracked as
	// in flight, nil if none.
	inFlightClaim *resourceapi.ResourceClaim
}

// change contains functions for modifying objects of a certain type. These
// functions will get called for all objects of that type. If they needs to
// make changes only to a particular instance, then it must check the name.
type change struct {
	scheduling func(*resourceapi.PodSchedulingContext) *resourceapi.PodSchedulingContext
	claim      func(*resourceapi.ResourceClaim) *resourceapi.ResourceClaim
}
type perNodeResult map[string]result

func (p perNodeResult) forNode(nodeName string) result {
	if p == nil {
		return result{}
	}
	return p[nodeName]
}

type want struct {
	preenqueue       result
	preFilterResult  *framework.PreFilterResult
	prefilter        result
	filter           perNodeResult
	prescore         result
	reserve          result
	unreserve        result
	prebind          result
	postbind         result
	postFilterResult *framework.PostFilterResult
	postfilter       result

	// unreserveAfterBindFailure, if set, triggers a call to Unreserve
	// after PreBind, as if the actual Bind had failed.
	unreserveAfterBindFailure *result

	// unreserveBeforePreBind, if set, triggers a call to Unreserve
	// before PreBind, as if the some other PreBind plugin had failed.
	unreserveBeforePreBind *result
}

// prepare contains changes for objects in the API server.
// Those changes are applied before running the steps. This can
// be used to simulate concurrent changes by some other entities
// like a resource driver.
type prepare struct {
	filter     change
	prescore   change
	reserve    change
	unreserve  change
	prebind    change
	postbind   change
	postfilter change
}

func TestPlugin(t *testing.T) {
	testcases := map[string]struct {
		nodes       []*v1.Node // default if unset is workerNode
		pod         *v1.Pod
		claims      []*resourceapi.ResourceClaim
		classes     []*resourceapi.DeviceClass
		schedulings []*resourceapi.PodSchedulingContext

		// objs get stored directly in the fake client, without passing
		// through reactors, in contrast to the types above.
		objs []apiruntime.Object

		prepare prepare
		want    want

		// Feature gates. False is chosen so that the uncommon case
		// doesn't need to be set.
		disableDRA        bool
		disableClassicDRA bool
	}{
		"empty": {
			pod: st.MakePod().Name("foo").Namespace("default").Obj(),
			want: want{
				prefilter: result{
					status: framework.NewStatus(framework.Skip),
				},
				postfilter: result{
					status: framework.NewStatus(framework.Unschedulable, `no new claims to deallocate`),
				},
			},
		},
		"claim-reference": {
			pod:    podWithClaimName,
			claims: []*resourceapi.ResourceClaim{allocatedClaim, otherClaim},
			want: want{
				prebind: result{
					changes: change{
						claim: func(claim *resourceapi.ResourceClaim) *resourceapi.ResourceClaim {
							if claim.Name == claimName {
								claim = claim.DeepCopy()
								claim.Status.ReservedFor = inUseClaim.Status.ReservedFor
							}
							return claim
						},
					},
				},
			},
		},
		"claim-reference-structured": {
			pod:    podWithClaimName,
			claims: []*resourceapi.ResourceClaim{structuredClaim(allocatedClaim), otherClaim},
			want: want{
				prebind: result{
					changes: change{
						claim: func(claim *resourceapi.ResourceClaim) *resourceapi.ResourceClaim {
							if claim.Name == claimName {
								claim = claim.DeepCopy()
								claim.Status.ReservedFor = inUseClaim.Status.ReservedFor
							}
							return claim
						},
					},
				},
			},
		},
		"claim-template": {
			pod:    podWithClaimTemplateInStatus,
			claims: []*resourceapi.ResourceClaim{allocatedClaim, otherClaim},
			want: want{
				prebind: result{
					changes: change{
						claim: func(claim *resourceapi.ResourceClaim) *resourceapi.ResourceClaim {
							if claim.Name == claimName {
								claim = claim.DeepCopy()
								claim.Status.ReservedFor = inUseClaim.Status.ReservedFor
							}
							return claim
						},
					},
				},
			},
		},
		"claim-template-structured": {
			pod:    podWithClaimTemplateInStatus,
			claims: []*resourceapi.ResourceClaim{structuredClaim(allocatedClaim), otherClaim},
			want: want{
				prebind: result{
					changes: change{
						claim: func(claim *resourceapi.ResourceClaim) *resourceapi.ResourceClaim {
							if claim.Name == claimName {
								claim = claim.DeepCopy()
								claim.Status.ReservedFor = inUseClaim.Status.ReservedFor
							}
							return claim
						},
					},
				},
			},
		},
		"missing-claim": {
			pod:    podWithClaimTemplate, // status not set
			claims: []*resourceapi.ResourceClaim{allocatedClaim, otherClaim},
			want: want{
				preenqueue: result{
					status: framework.NewStatus(framework.UnschedulableAndUnresolvable, `pod "default/my-pod": ResourceClaim not created yet`),
				},
			},
		},
		"deleted-claim": {
			pod: podWithClaimTemplateInStatus,
			claims: func() []*resourceapi.ResourceClaim {
				claim := allocatedClaim.DeepCopy()
				claim.DeletionTimestamp = &metav1.Time{Time: time.Now()}
				return []*resourceapi.ResourceClaim{claim}
			}(),
			want: want{
				preenqueue: result{
					status: framework.NewStatus(framework.UnschedulableAndUnresolvable, `resourceclaim "my-pod-my-resource" is being deleted`),
				},
			},
		},
		"wrong-claim": {
			pod: podWithClaimTemplateInStatus,
			claims: func() []*resourceapi.ResourceClaim {
				claim := allocatedClaim.DeepCopy()
				claim.OwnerReferences[0].UID += "123"
				return []*resourceapi.ResourceClaim{claim}
			}(),
			want: want{
				preenqueue: result{
					status: framework.NewStatus(framework.UnschedulableAndUnresolvable, `ResourceClaim default/my-pod-my-resource was not created for pod default/my-pod (pod is not owner)`),
				},
			},
		},
		"structured-no-resources": {
			pod:     podWithClaimName,
			claims:  []*resourceapi.ResourceClaim{structuredClaim(pendingClaim)},
			classes: []*resourceapi.DeviceClass{deviceClass},
			want: want{
				filter: perNodeResult{
					workerNode.Name: {
						status: framework.NewStatus(framework.UnschedulableAndUnresolvable, `cannot allocate all claims`),
					},
				},
				postfilter: result{
					status: framework.NewStatus(framework.Unschedulable, `still not schedulable`),
				},
			},
		},
		"structured-with-resources": {
			pod:     podWithClaimName,
			claims:  []*resourceapi.ResourceClaim{structuredClaim(pendingClaim)},
			classes: []*resourceapi.DeviceClass{deviceClass},
			objs:    []apiruntime.Object{workerNodeSlice},
			want: want{
				reserve: result{
					inFlightClaim: structuredClaim(allocatedClaim),
				},
				prebind: result{
					assumedClaim: reserve(structuredClaim(allocatedClaim), podWithClaimName),
					changes: change{
						claim: func(claim *resourceapi.ResourceClaim) *resourceapi.ResourceClaim {
							if claim.Name == claimName {
								claim = claim.DeepCopy()
								claim.Finalizers = structuredClaim(allocatedClaim).Finalizers
								claim.Status = structuredClaim(inUseClaim).Status
							}
							return claim
						},
					},
				},
				postbind: result{
					assumedClaim: reserve(structuredClaim(allocatedClaim), podWithClaimName),
				},
			},
		},
		"structured-with-resources-has-finalizer": {
			// As before. but the finalizer is already set. Could happen if
			// the scheduler got interrupted.
			pod: podWithClaimName,
			claims: func() []*resourceapi.ResourceClaim {
				claim := structuredClaim(pendingClaim)
				claim.Finalizers = structuredClaim(allocatedClaim).Finalizers
				return []*resourceapi.ResourceClaim{claim}
			}(),
			classes: []*resourceapi.DeviceClass{deviceClass},
			objs:    []apiruntime.Object{workerNodeSlice},
			want: want{
				reserve: result{
					inFlightClaim: structuredClaim(allocatedClaim),
				},
				prebind: result{
					assumedClaim: reserve(structuredClaim(allocatedClaim), podWithClaimName),
					changes: change{
						claim: func(claim *resourceapi.ResourceClaim) *resourceapi.ResourceClaim {
							if claim.Name == claimName {
								claim = claim.DeepCopy()
								claim.Status = structuredInUseClaim.Status
							}
							return claim
						},
					},
				},
				postbind: result{
					assumedClaim: reserve(structuredClaim(allocatedClaim), podWithClaimName),
				},
			},
		},
		"structured-with-resources-finalizer-gets-removed": {
			// As before. but the finalizer is already set. Then it gets
			// removed before the scheduler reaches PreBind.
			pod: podWithClaimName,
			claims: func() []*resourceapi.ResourceClaim {
				claim := structuredClaim(pendingClaim)
				claim.Finalizers = structuredClaim(allocatedClaim).Finalizers
				return []*resourceapi.ResourceClaim{claim}
			}(),
			classes: []*resourceapi.DeviceClass{deviceClass},
			objs:    []apiruntime.Object{workerNodeSlice},
			prepare: prepare{
				prebind: change{
					claim: func(claim *resourceapi.ResourceClaim) *resourceapi.ResourceClaim {
						claim.Finalizers = nil
						return claim
					},
				},
			},
			want: want{
				reserve: result{
					inFlightClaim: structuredClaim(allocatedClaim),
				},
				prebind: result{
					assumedClaim: reserve(structuredClaim(allocatedClaim), podWithClaimName),
					changes: change{
						claim: func(claim *resourceapi.ResourceClaim) *resourceapi.ResourceClaim {
							if claim.Name == claimName {
								claim = claim.DeepCopy()
								claim.Finalizers = structuredClaim(allocatedClaim).Finalizers
								claim.Status = structuredInUseClaim.Status
							}
							return claim
						},
					},
				},
				postbind: result{
					assumedClaim: reserve(structuredClaim(allocatedClaim), podWithClaimName),
				},
			},
		},
		"structured-with-resources-finalizer-gets-added": {
			// No finalizer initially, then it gets added before
			// the scheduler reaches PreBind. Shouldn't happen?
			pod:     podWithClaimName,
			claims:  []*resourceapi.ResourceClaim{structuredClaim(pendingClaim)},
			classes: []*resourceapi.DeviceClass{deviceClass},
			objs:    []apiruntime.Object{workerNodeSlice},
			prepare: prepare{
				prebind: change{
					claim: func(claim *resourceapi.ResourceClaim) *resourceapi.ResourceClaim {
						claim.Finalizers = structuredClaim(allocatedClaim).Finalizers
						return claim
					},
				},
			},
			want: want{
				reserve: result{
					inFlightClaim: structuredClaim(allocatedClaim),
				},
				prebind: result{
					assumedClaim: reserve(structuredClaim(allocatedClaim), podWithClaimName),
					changes: change{
						claim: func(claim *resourceapi.ResourceClaim) *resourceapi.ResourceClaim {
							if claim.Name == claimName {
								claim = claim.DeepCopy()
								claim.Status = structuredInUseClaim.Status
							}
							return claim
						},
					},
				},
				postbind: result{
					assumedClaim: reserve(structuredClaim(allocatedClaim), podWithClaimName),
				},
			},
		},
		"structured-skip-bind": {
			pod:     podWithClaimName,
			claims:  []*resourceapi.ResourceClaim{structuredClaim(pendingClaim)},
			classes: []*resourceapi.DeviceClass{deviceClass},
			objs:    []apiruntime.Object{workerNodeSlice},
			want: want{
				reserve: result{
					inFlightClaim: structuredClaim(allocatedClaim),
				},
				unreserveBeforePreBind: &result{},
			},
		},
		"structured-exhausted-resources": {
			pod:     podWithClaimName,
			claims:  []*resourceapi.ResourceClaim{structuredClaim(pendingClaim), structuredClaim(otherAllocatedClaim)},
			classes: []*resourceapi.DeviceClass{deviceClass},
			objs:    []apiruntime.Object{workerNodeSlice},
			want: want{
				filter: perNodeResult{
					workerNode.Name: {
						status: framework.NewStatus(framework.UnschedulableAndUnresolvable, `cannot allocate all claims`),
					},
				},
				postfilter: result{
					status: framework.NewStatus(framework.Unschedulable, `still not schedulable`),
				},
			},
		},

		"claim-parameters-CEL-runtime-error": {
			pod:     podWithClaimName,
			claims:  []*resourceapi.ResourceClaim{breakCELInClaim(structuredClaim(pendingClaim))},
			classes: []*resourceapi.DeviceClass{deviceClass},
			objs:    []apiruntime.Object{workerNodeSlice},
			want: want{
				filter: perNodeResult{
					workerNode.Name: {
						status: framework.AsStatus(errors.New(`claim default/my-pod-my-resource: selector #0: CEL runtime error: no such key: ` + string(attrName))),
					},
				},
			},
		},

		"class-parameters-CEL-runtime-error": {
			pod:     podWithClaimName,
			claims:  []*resourceapi.ResourceClaim{structuredClaim(pendingClaim)},
			classes: []*resourceapi.DeviceClass{breakCELInClass(deviceClass)},
			objs:    []apiruntime.Object{workerNodeSlice},
			want: want{
				filter: perNodeResult{
					workerNode.Name: {
						status: framework.AsStatus(errors.New(`class my-resource-class: selector #0: CEL runtime error: no such key: ` + string(attrName))),
					},
				},
			},
		},

		// When pod scheduling encounters CEL runtime errors for some nodes, but not all,
		// it should still not schedule the pod because there is something wrong with it.
		// Scheduling it would make it harder to detect that there is a problem.
		//
		// This matches the "keeps pod pending because of CEL runtime errors" E2E test.
		"CEL-runtime-error-for-one-of-two-nodes": {
			nodes:   []*v1.Node{workerNode, workerNode2},
			pod:     podWithClaimName,
			claims:  []*resourceapi.ResourceClaim{breakCELInClaim(structuredClaim(pendingClaim))},
			classes: []*resourceapi.DeviceClass{deviceClass},
			objs:    []apiruntime.Object{workerNodeSlice, workerNode2Slice},
			want: want{
				filter: perNodeResult{
					workerNode.Name: {
						status: framework.AsStatus(errors.New(`claim default/my-pod-my-resource: selector #0: CEL runtime error: no such key: ` + string(attrName))),
					},
				},
			},
		},

		// When two nodes where found, PreScore gets called.
		"CEL-runtime-error-for-one-of-three-nodes": {
			nodes:   []*v1.Node{workerNode, workerNode2, workerNode3},
			pod:     podWithClaimName,
			claims:  []*resourceapi.ResourceClaim{breakCELInClaim(structuredClaim(pendingClaim))},
			classes: []*resourceapi.DeviceClass{deviceClass},
			objs:    []apiruntime.Object{workerNodeSlice, workerNode2Slice, workerNode3Slice},
			want: want{
				filter: perNodeResult{
					workerNode.Name: {
						status: framework.NewStatus(framework.UnschedulableAndUnresolvable, `claim default/my-pod-my-resource: selector #0: CEL runtime error: no such key: `+string(attrName)),
					},
				},
				prescore: result{
					// This is the error found during Filter.
					status: framework.NewStatus(framework.UnschedulableAndUnresolvable, `filter node worker: claim default/my-pod-my-resource: selector #0: CEL runtime error: no such key: healthy`),
				},
			},
		},

		"waiting-for-deallocation": {
			pod:    podWithClaimName,
			claims: []*resourceapi.ResourceClaim{deallocatingClaim},
			want: want{
				prefilter: result{
					status: framework.NewStatus(framework.UnschedulableAndUnresolvable, `resourceclaim must be reallocated`),
				},
				postfilter: result{
					status: framework.NewStatus(framework.Unschedulable, `no new claims to deallocate`),
				},
			},
		},
		"missing-class": {
			pod:    podWithClaimName,
			claims: []*resourceapi.ResourceClaim{pendingClaim},
			want: want{
				prefilter: result{
					status: framework.NewStatus(framework.UnschedulableAndUnresolvable, fmt.Sprintf("request req-1: device class %s does not exist", className)),
				},
				postfilter: result{
					status: framework.NewStatus(framework.Unschedulable, `no new claims to deallocate`),
				},
			},
		},
		"scheduling-select-immediately": {
			// Create the PodSchedulingContext object, ask for information
			// and select a node.
			pod:     podWithClaimName,
			claims:  []*resourceapi.ResourceClaim{pendingClaim},
			classes: []*resourceapi.DeviceClass{deviceClass},
			want: want{
				prebind: result{
					status: framework.NewStatus(framework.Pending, `waiting for resource driver`),
					added:  []metav1.Object{schedulingSelectedPotential},
				},
			},
		},
		"scheduling-ask": {
			// Create the PodSchedulingContext object, ask for
			// information, but do not select a node because
			// there are multiple claims.
			pod:     podWithTwoClaimNames,
			claims:  []*resourceapi.ResourceClaim{pendingClaim, pendingClaim2},
			classes: []*resourceapi.DeviceClass{deviceClass},
			want: want{
				prebind: result{
					status: framework.NewStatus(framework.Pending, `waiting for resource driver`),
					added:  []metav1.Object{schedulingPotential},
				},
			},
		},
		"scheduling-finish": {
			// Use the populated PodSchedulingContext object to select a
			// node.
			pod:         podWithClaimName,
			claims:      []*resourceapi.ResourceClaim{pendingClaim},
			schedulings: []*resourceapi.PodSchedulingContext{schedulingInfo},
			classes:     []*resourceapi.DeviceClass{deviceClass},
			want: want{
				prebind: result{
					status: framework.NewStatus(framework.Pending, `waiting for resource driver`),
					changes: change{
						scheduling: func(in *resourceapi.PodSchedulingContext) *resourceapi.PodSchedulingContext {
							return st.FromPodSchedulingContexts(in).
								SelectedNode(workerNode.Name).
								Obj()
						},
					},
				},
			},
		},
		"scheduling-finish-concurrent-label-update": {
			// Use the populated PodSchedulingContext object to select a
			// node.
			pod:         podWithClaimName,
			claims:      []*resourceapi.ResourceClaim{pendingClaim},
			schedulings: []*resourceapi.PodSchedulingContext{schedulingInfo},
			classes:     []*resourceapi.DeviceClass{deviceClass},
			prepare: prepare{
				prebind: change{
					scheduling: func(in *resourceapi.PodSchedulingContext) *resourceapi.PodSchedulingContext {
						// This does not actually conflict with setting the
						// selected node, but because the plugin is not using
						// patching yet, Update nonetheless fails.
						return st.FromPodSchedulingContexts(in).
							Label("hello", "world").
							Obj()
					},
				},
			},
			want: want{
				prebind: result{
					status: framework.AsStatus(errors.New(`ResourceVersion must match the object that gets updated`)),
				},
			},
		},
		"scheduling-completed": {
			// Remove PodSchedulingContext object once the pod is scheduled.
			pod:         podWithClaimName,
			claims:      []*resourceapi.ResourceClaim{allocatedClaim},
			schedulings: []*resourceapi.PodSchedulingContext{schedulingInfo},
			classes:     []*resourceapi.DeviceClass{deviceClass},
			want: want{
				prebind: result{
					changes: change{
						claim: func(in *resourceapi.ResourceClaim) *resourceapi.ResourceClaim {
							return st.FromResourceClaim(in).
								ReservedFor(resourceapi.ResourceClaimConsumerReference{Resource: "pods", Name: podName, UID: types.UID(podUID)}).
								Obj()
						},
					},
				},
				postbind: result{
					removed: []metav1.Object{schedulingInfo},
				},
			},
		},
		"wrong-topology": {
			// PostFilter tries to get the pod scheduleable by
			// deallocating the claim.
			pod:    podWithClaimName,
			claims: []*resourceapi.ResourceClaim{allocatedClaimWithWrongTopology},
			want: want{
				filter: perNodeResult{
					workerNode.Name: {
						status: framework.NewStatus(framework.UnschedulableAndUnresolvable, `resourceclaim not available on the node`),
					},
				},
				postfilter: result{
					// Claims with delayed allocation get deallocated.
					changes: change{
						claim: func(in *resourceapi.ResourceClaim) *resourceapi.ResourceClaim {
							return st.FromResourceClaim(in).
								DeallocationRequested(true).
								Obj()
						},
					},
					status: framework.NewStatus(framework.Unschedulable, `deallocation of ResourceClaim completed`),
				},
			},
		},
		"wrong-topology-structured": {
			// PostFilter tries to get the pod scheduleable by
			// deallocating the claim.
			pod:    podWithClaimName,
			claims: []*resourceapi.ResourceClaim{structuredClaim(allocatedClaimWithWrongTopology)},
			want: want{
				filter: perNodeResult{
					workerNode.Name: {
						status: framework.NewStatus(framework.UnschedulableAndUnresolvable, `resourceclaim not available on the node`),
					},
				},
				postfilter: result{
					// Claims with delayed allocation and structured parameters get deallocated immediately.
					changes: change{
						claim: func(in *resourceapi.ResourceClaim) *resourceapi.ResourceClaim {
							return st.FromResourceClaim(in).
								Allocation(nil).
								Obj()
						},
					},
					status: framework.NewStatus(framework.Unschedulable, `deallocation of ResourceClaim completed`),
				},
			},
		},
		"good-topology": {
			pod:    podWithClaimName,
			claims: []*resourceapi.ResourceClaim{allocatedClaimWithGoodTopology},
			want: want{
				prebind: result{
					changes: change{
						claim: func(in *resourceapi.ResourceClaim) *resourceapi.ResourceClaim {
							return st.FromResourceClaim(in).
								ReservedFor(resourceapi.ResourceClaimConsumerReference{Resource: "pods", Name: podName, UID: types.UID(podUID)}).
								Obj()
						},
					},
				},
			},
		},
		"bind-failure": {
			pod:    podWithClaimName,
			claims: []*resourceapi.ResourceClaim{allocatedClaimWithGoodTopology},
			want: want{
				prebind: result{
					changes: change{
						claim: func(in *resourceapi.ResourceClaim) *resourceapi.ResourceClaim {
							return st.FromResourceClaim(in).
								ReservedFor(resourceapi.ResourceClaimConsumerReference{Resource: "pods", Name: podName, UID: types.UID(podUID)}).
								Obj()
						},
					},
				},
				unreserveAfterBindFailure: &result{
					changes: change{
						claim: func(in *resourceapi.ResourceClaim) *resourceapi.ResourceClaim {
							out := in.DeepCopy()
							out.Status.ReservedFor = []resourceapi.ResourceClaimConsumerReference{}
							return out
						},
					},
				},
			},
		},
		"bind-failure-structured": {
			pod:    podWithClaimName,
			claims: []*resourceapi.ResourceClaim{structuredClaim(allocatedClaimWithGoodTopology)},
			want: want{
				prebind: result{
					changes: change{
						claim: func(in *resourceapi.ResourceClaim) *resourceapi.ResourceClaim {
							return st.FromResourceClaim(in).
								ReservedFor(resourceapi.ResourceClaimConsumerReference{Resource: "pods", Name: podName, UID: types.UID(podUID)}).
								Obj()
						},
					},
				},
				unreserveAfterBindFailure: &result{
					changes: change{
						claim: func(in *resourceapi.ResourceClaim) *resourceapi.ResourceClaim {
							out := in.DeepCopy()
							out.Status.ReservedFor = []resourceapi.ResourceClaimConsumerReference{}
							return out
						},
					},
				},
			},
		},
		"reserved-okay": {
			pod:    podWithClaimName,
			claims: []*resourceapi.ResourceClaim{inUseClaim},
		},
		"DRA-disabled": {
			pod:    podWithClaimName,
			claims: []*resourceapi.ResourceClaim{inUseClaim},
			want: want{
				prefilter: result{
					status: framework.NewStatus(framework.Skip),
				},
			},
			disableDRA: true,
		},
	}

	for name, tc := range testcases {
		// We can run in parallel because logging is per-test.
		tc := tc
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			nodes := tc.nodes
			if nodes == nil {
				nodes = []*v1.Node{workerNode}
			}
			features := feature.Features{
				EnableDynamicResourceAllocation: !tc.disableDRA,
				EnableDRAControlPlaneController: !tc.disableClassicDRA,
			}
			testCtx := setup(t, nodes, tc.claims, tc.classes, tc.schedulings, tc.objs, features)
			initialObjects := testCtx.listAll(t)

			status := testCtx.p.PreEnqueue(testCtx.ctx, tc.pod)
			t.Run("PreEnqueue", func(t *testing.T) {
				testCtx.verify(t, tc.want.preenqueue, initialObjects, nil, status)
			})
			if !status.IsSuccess() {
				return
			}

			result, status := testCtx.p.PreFilter(testCtx.ctx, testCtx.state, tc.pod)
			t.Run("prefilter", func(t *testing.T) {
				assert.Equal(t, tc.want.preFilterResult, result)
				testCtx.verify(t, tc.want.prefilter, initialObjects, result, status)
			})
			if status.IsSkip() {
				return
			}
			unschedulable := status.Code() != framework.Success

			var potentialNodes []*framework.NodeInfo

			initialObjects = testCtx.listAll(t)
			testCtx.updateAPIServer(t, initialObjects, tc.prepare.filter)
			if !unschedulable {
				for _, nodeInfo := range testCtx.nodeInfos {
					initialObjects = testCtx.listAll(t)
					status := testCtx.p.Filter(testCtx.ctx, testCtx.state, tc.pod, nodeInfo)
					nodeName := nodeInfo.Node().Name
					t.Run(fmt.Sprintf("filter/%s", nodeInfo.Node().Name), func(t *testing.T) {
						testCtx.verify(t, tc.want.filter.forNode(nodeName), initialObjects, nil, status)
					})
					if status.Code() == framework.Success {
						potentialNodes = append(potentialNodes, nodeInfo)
					}
					if status.Code() == framework.Error {
						// An error aborts scheduling.
						return
					}
				}
				if len(potentialNodes) == 0 {
					unschedulable = true
				}
			}

			if !unschedulable && len(potentialNodes) > 1 {
				initialObjects = testCtx.listAll(t)
				initialObjects = testCtx.updateAPIServer(t, initialObjects, tc.prepare.prescore)
				status := testCtx.p.PreScore(testCtx.ctx, testCtx.state, tc.pod, potentialNodes)
				t.Run("prescore", func(t *testing.T) {
					testCtx.verify(t, tc.want.prescore, initialObjects, nil, status)
				})
				if status.Code() != framework.Success {
					unschedulable = true
				}
			}

			var selectedNode *framework.NodeInfo
			if !unschedulable && len(potentialNodes) > 0 {
				selectedNode = potentialNodes[0]

				initialObjects = testCtx.listAll(t)
				initialObjects = testCtx.updateAPIServer(t, initialObjects, tc.prepare.reserve)
				status := testCtx.p.Reserve(testCtx.ctx, testCtx.state, tc.pod, selectedNode.Node().Name)
				t.Run("reserve", func(t *testing.T) {
					testCtx.verify(t, tc.want.reserve, initialObjects, nil, status)
				})
				if status.Code() != framework.Success {
					unschedulable = true
				}
			}

			if selectedNode != nil {
				if unschedulable {
					initialObjects = testCtx.listAll(t)
					initialObjects = testCtx.updateAPIServer(t, initialObjects, tc.prepare.unreserve)
					testCtx.p.Unreserve(testCtx.ctx, testCtx.state, tc.pod, selectedNode.Node().Name)
					t.Run("unreserve", func(t *testing.T) {
						testCtx.verify(t, tc.want.unreserve, initialObjects, nil, status)
					})
				} else {
					if tc.want.unreserveBeforePreBind != nil {
						initialObjects = testCtx.listAll(t)
						testCtx.p.Unreserve(testCtx.ctx, testCtx.state, tc.pod, selectedNode.Node().Name)
						t.Run("unreserveBeforePreBind", func(t *testing.T) {
							testCtx.verify(t, *tc.want.unreserveBeforePreBind, initialObjects, nil, status)
						})
						return
					}

					initialObjects = testCtx.listAll(t)
					initialObjects = testCtx.updateAPIServer(t, initialObjects, tc.prepare.prebind)
					status := testCtx.p.PreBind(testCtx.ctx, testCtx.state, tc.pod, selectedNode.Node().Name)
					t.Run("prebind", func(t *testing.T) {
						testCtx.verify(t, tc.want.prebind, initialObjects, nil, status)
					})

					if tc.want.unreserveAfterBindFailure != nil {
						initialObjects = testCtx.listAll(t)
						testCtx.p.Unreserve(testCtx.ctx, testCtx.state, tc.pod, selectedNode.Node().Name)
						t.Run("unreserverAfterBindFailure", func(t *testing.T) {
							testCtx.verify(t, *tc.want.unreserveAfterBindFailure, initialObjects, nil, status)
						})
					} else if status.IsSuccess() {
						initialObjects = testCtx.listAll(t)
						initialObjects = testCtx.updateAPIServer(t, initialObjects, tc.prepare.postbind)
						testCtx.p.PostBind(testCtx.ctx, testCtx.state, tc.pod, selectedNode.Node().Name)
						t.Run("postbind", func(t *testing.T) {
							testCtx.verify(t, tc.want.postbind, initialObjects, nil, nil)
						})
					}
				}
			} else if len(potentialNodes) == 0 {
				initialObjects = testCtx.listAll(t)
				initialObjects = testCtx.updateAPIServer(t, initialObjects, tc.prepare.postfilter)
				result, status := testCtx.p.PostFilter(testCtx.ctx, testCtx.state, tc.pod, nil /* filteredNodeStatusMap not used by plugin */)
				t.Run("postfilter", func(t *testing.T) {
					assert.Equal(t, tc.want.postFilterResult, result)
					testCtx.verify(t, tc.want.postfilter, initialObjects, nil, status)
				})
			}
		})
	}
}

type testContext struct {
	ctx              context.Context
	client           *fake.Clientset
	informerFactory  informers.SharedInformerFactory
	claimAssumeCache *assumecache.AssumeCache
	p                *dynamicResources
	nodeInfos        []*framework.NodeInfo
	state            *framework.CycleState
}

func (tc *testContext) verify(t *testing.T, expected result, initialObjects []metav1.Object, result interface{}, status *framework.Status) {
	t.Helper()
	if expectedErr := status.AsError(); expectedErr != nil {
		// Compare only the error strings.
		assert.ErrorContains(t, status.AsError(), expectedErr.Error())
	} else {
		assert.Equal(t, expected.status, status)
	}
	objects := tc.listAll(t)
	wantObjects := update(t, initialObjects, expected.changes)
	wantObjects = append(wantObjects, expected.added...)
	for _, remove := range expected.removed {
		for i, obj := range wantObjects {
			// This is a bit relaxed (no GVR comparison, no UID
			// comparison) to simplify writing the test cases.
			if obj.GetName() == remove.GetName() && obj.GetNamespace() == remove.GetNamespace() {
				wantObjects = append(wantObjects[0:i], wantObjects[i+1:]...)
				break
			}
		}
	}
	sortObjects(wantObjects)
	// Sometimes assert strips the diff too much, let's do it ourselves...
	if diff := cmp.Diff(wantObjects, objects, cmpopts.IgnoreFields(metav1.ObjectMeta{}, "UID", "ResourceVersion")); diff != "" {
		t.Errorf("Stored objects are different (- expected, + actual):\n%s", diff)
	}

	var expectAssumedClaims []metav1.Object
	if expected.assumedClaim != nil {
		expectAssumedClaims = append(expectAssumedClaims, expected.assumedClaim)
	}
	actualAssumedClaims := tc.listAssumedClaims()
	if diff := cmp.Diff(expectAssumedClaims, actualAssumedClaims, cmpopts.IgnoreFields(metav1.ObjectMeta{}, "UID", "ResourceVersion")); diff != "" {
		t.Errorf("Assumed claims are different (- expected, + actual):\n%s", diff)
	}

	var expectInFlightClaims []metav1.Object
	if expected.inFlightClaim != nil {
		expectInFlightClaims = append(expectInFlightClaims, expected.inFlightClaim)
	}
	actualInFlightClaims := tc.listInFlightClaims()
	if diff := cmp.Diff(expectInFlightClaims, actualInFlightClaims, cmpopts.IgnoreFields(metav1.ObjectMeta{}, "UID", "ResourceVersion")); diff != "" {
		t.Errorf("In-flight claims are different (- expected, + actual):\n%s", diff)
	}
}

func (tc *testContext) listAll(t *testing.T) (objects []metav1.Object) {
	t.Helper()
	claims, err := tc.client.ResourceV1alpha3().ResourceClaims("").List(tc.ctx, metav1.ListOptions{})
	require.NoError(t, err, "list claims")
	for _, claim := range claims.Items {
		claim := claim
		objects = append(objects, &claim)
	}
	schedulings, err := tc.client.ResourceV1alpha3().PodSchedulingContexts("").List(tc.ctx, metav1.ListOptions{})
	require.NoError(t, err, "list pod scheduling")
	for _, scheduling := range schedulings.Items {
		scheduling := scheduling
		objects = append(objects, &scheduling)
	}

	sortObjects(objects)
	return
}

func (tc *testContext) listAssumedClaims() []metav1.Object {
	if tc.p.claimAssumeCache == nil {
		return nil
	}
	var assumedClaims []metav1.Object
	for _, obj := range tc.p.claimAssumeCache.List(nil) {
		claim := obj.(*resourceapi.ResourceClaim)
		obj, _ := tc.p.claimAssumeCache.Get(claim.Namespace + "/" + claim.Name)
		apiObj, _ := tc.p.claimAssumeCache.GetAPIObj(claim.Namespace + "/" + claim.Name)
		if obj != apiObj {
			assumedClaims = append(assumedClaims, claim)
		}
	}
	sortObjects(assumedClaims)
	return assumedClaims
}

func (tc *testContext) listInFlightClaims() []metav1.Object {
	var inFlightClaims []metav1.Object
	tc.p.inFlightAllocations.Range(func(key, value any) bool {
		inFlightClaims = append(inFlightClaims, value.(*resourceapi.ResourceClaim))
		return true
	})
	sortObjects(inFlightClaims)
	return inFlightClaims
}

// updateAPIServer modifies objects and stores any changed object in the API server.
func (tc *testContext) updateAPIServer(t *testing.T, objects []metav1.Object, updates change) []metav1.Object {
	modified := update(t, objects, updates)
	for i := range modified {
		obj := modified[i]
		if diff := cmp.Diff(objects[i], obj); diff != "" {
			t.Logf("Updating %T %q, diff (-old, +new):\n%s", obj, obj.GetName(), diff)
			switch obj := obj.(type) {
			case *resourceapi.ResourceClaim:
				obj, err := tc.client.ResourceV1alpha3().ResourceClaims(obj.Namespace).Update(tc.ctx, obj, metav1.UpdateOptions{})
				if err != nil {
					t.Fatalf("unexpected error during prepare update: %v", err)
				}
				modified[i] = obj
			case *resourceapi.PodSchedulingContext:
				obj, err := tc.client.ResourceV1alpha3().PodSchedulingContexts(obj.Namespace).Update(tc.ctx, obj, metav1.UpdateOptions{})
				if err != nil {
					t.Fatalf("unexpected error during prepare update: %v", err)
				}
				modified[i] = obj
			default:
				t.Fatalf("unsupported object type %T", obj)
			}
		}
	}
	return modified
}

func sortObjects(objects []metav1.Object) {
	sort.Slice(objects, func(i, j int) bool {
		if objects[i].GetNamespace() < objects[j].GetNamespace() {
			return true
		}
		return objects[i].GetName() < objects[j].GetName()
	})
}

// update walks through all existing objects, finds the corresponding update
// function based on name and kind, and replaces those objects that have an
// update function. The rest is left unchanged.
func update(t *testing.T, objects []metav1.Object, updates change) []metav1.Object {
	var updated []metav1.Object

	for _, obj := range objects {
		switch in := obj.(type) {
		case *resourceapi.ResourceClaim:
			if updates.claim != nil {
				obj = updates.claim(in)
			}
		case *resourceapi.PodSchedulingContext:
			if updates.scheduling != nil {
				obj = updates.scheduling(in)
			}
		}
		updated = append(updated, obj)
	}

	return updated
}

func setup(t *testing.T, nodes []*v1.Node, claims []*resourceapi.ResourceClaim, classes []*resourceapi.DeviceClass, schedulings []*resourceapi.PodSchedulingContext, objs []apiruntime.Object, features feature.Features) (result *testContext) {
	t.Helper()

	tc := &testContext{}
	tCtx := ktesting.Init(t)
	tc.ctx = tCtx

	tc.client = fake.NewSimpleClientset(objs...)
	reactor := createReactor(tc.client.Tracker())
	tc.client.PrependReactor("*", "*", reactor)

	tc.informerFactory = informers.NewSharedInformerFactory(tc.client, 0)
	tc.claimAssumeCache = assumecache.NewAssumeCache(tCtx.Logger(), tc.informerFactory.Resource().V1alpha3().ResourceClaims().Informer(), "resource claim", "", nil)
	opts := []runtime.Option{
		runtime.WithClientSet(tc.client),
		runtime.WithInformerFactory(tc.informerFactory),
		runtime.WithResourceClaimCache(tc.claimAssumeCache),
	}
	fh, err := runtime.NewFramework(tCtx, nil, nil, opts...)
	if err != nil {
		t.Fatal(err)
	}

	pl, err := New(tCtx, nil, fh, features)
	if err != nil {
		t.Fatal(err)
	}
	tc.p = pl.(*dynamicResources)

	// The tests use the API to create the objects because then reactors
	// get triggered.
	for _, claim := range claims {
		_, err := tc.client.ResourceV1alpha3().ResourceClaims(claim.Namespace).Create(tc.ctx, claim, metav1.CreateOptions{})
		require.NoError(t, err, "create resource claim")
	}
	for _, class := range classes {
		_, err := tc.client.ResourceV1alpha3().DeviceClasses().Create(tc.ctx, class, metav1.CreateOptions{})
		require.NoError(t, err, "create resource class")
	}
	for _, scheduling := range schedulings {
		_, err := tc.client.ResourceV1alpha3().PodSchedulingContexts(scheduling.Namespace).Create(tc.ctx, scheduling, metav1.CreateOptions{})
		require.NoError(t, err, "create pod scheduling")
	}

	tc.informerFactory.Start(tc.ctx.Done())
	t.Cleanup(func() {
		// Need to cancel before waiting for the shutdown.
		tCtx.Cancel("test is done")
		// Now we can wait for all goroutines to stop.
		tc.informerFactory.Shutdown()
	})

	tc.informerFactory.WaitForCacheSync(tc.ctx.Done())

	for _, node := range nodes {
		nodeInfo := framework.NewNodeInfo()
		nodeInfo.SetNode(node)
		tc.nodeInfos = append(tc.nodeInfos, nodeInfo)
	}
	tc.state = framework.NewCycleState()

	return tc
}

// createReactor implements the logic required for the UID and ResourceVersion
// fields to work when using the fake client. Add it with client.PrependReactor
// to your fake client. ResourceVersion handling is required for conflict
// detection during updates, which is covered by some scenarios.
func createReactor(tracker cgotesting.ObjectTracker) func(action cgotesting.Action) (handled bool, ret apiruntime.Object, err error) {
	var uidCounter int
	var resourceVersionCounter int
	var mutex sync.Mutex

	return func(action cgotesting.Action) (handled bool, ret apiruntime.Object, err error) {
		createAction, ok := action.(cgotesting.CreateAction)
		if !ok {
			return false, nil, nil
		}
		obj, ok := createAction.GetObject().(metav1.Object)
		if !ok {
			return false, nil, nil
		}

		mutex.Lock()
		defer mutex.Unlock()
		switch action.GetVerb() {
		case "create":
			if obj.GetUID() != "" {
				return true, nil, errors.New("UID must not be set on create")
			}
			if obj.GetResourceVersion() != "" {
				return true, nil, errors.New("ResourceVersion must not be set on create")
			}
			obj.SetUID(types.UID(fmt.Sprintf("UID-%d", uidCounter)))
			uidCounter++
			obj.SetResourceVersion(fmt.Sprintf("%d", resourceVersionCounter))
			resourceVersionCounter++
		case "update":
			uid := obj.GetUID()
			resourceVersion := obj.GetResourceVersion()
			if uid == "" {
				return true, nil, errors.New("UID must be set on update")
			}
			if resourceVersion == "" {
				return true, nil, errors.New("ResourceVersion must be set on update")
			}

			oldObj, err := tracker.Get(action.GetResource(), obj.GetNamespace(), obj.GetName())
			if err != nil {
				return true, nil, err
			}
			oldObjMeta, ok := oldObj.(metav1.Object)
			if !ok {
				return true, nil, errors.New("internal error: unexpected old object type")
			}
			if oldObjMeta.GetResourceVersion() != resourceVersion {
				return true, nil, errors.New("ResourceVersion must match the object that gets updated")
			}

			obj.SetResourceVersion(fmt.Sprintf("%d", resourceVersionCounter))
			resourceVersionCounter++
		}
		return false, nil, nil
	}
}

func Test_isSchedulableAfterClaimChange(t *testing.T) {
	testcases := map[string]struct {
		pod            *v1.Pod
		claims         []*resourceapi.ResourceClaim
		oldObj, newObj interface{}
		expectedHint   framework.QueueingHint
		expectedErr    bool
	}{
		"skip-deletes": {
			pod:          podWithClaimTemplate,
			oldObj:       allocatedClaim,
			newObj:       nil,
			expectedHint: framework.QueueSkip,
		},
		"backoff-wrong-new-object": {
			pod:         podWithClaimTemplate,
			newObj:      "not-a-claim",
			expectedErr: true,
		},
		"skip-wrong-claim": {
			pod: podWithClaimTemplate,
			newObj: func() *resourceapi.ResourceClaim {
				claim := allocatedClaim.DeepCopy()
				claim.OwnerReferences[0].UID += "123"
				return claim
			}(),
			expectedHint: framework.QueueSkip,
		},
		"skip-unrelated-claim": {
			pod:    podWithClaimTemplate,
			claims: []*resourceapi.ResourceClaim{allocatedClaim},
			newObj: func() *resourceapi.ResourceClaim {
				claim := allocatedClaim.DeepCopy()
				claim.Name += "-foo"
				claim.UID += "123"
				return claim
			}(),
			expectedHint: framework.QueueSkip,
		},
		"queue-on-add": {
			pod:          podWithClaimName,
			newObj:       pendingClaim,
			expectedHint: framework.Queue,
		},
		"backoff-wrong-old-object": {
			pod:         podWithClaimName,
			claims:      []*resourceapi.ResourceClaim{pendingClaim},
			oldObj:      "not-a-claim",
			newObj:      pendingClaim,
			expectedErr: true,
		},
		"skip-adding-finalizer": {
			pod:    podWithClaimName,
			claims: []*resourceapi.ResourceClaim{pendingClaim},
			oldObj: pendingClaim,
			newObj: func() *resourceapi.ResourceClaim {
				claim := pendingClaim.DeepCopy()
				claim.Finalizers = append(claim.Finalizers, "foo")
				return claim
			}(),
			expectedHint: framework.QueueSkip,
		},
		"queue-on-status-change": {
			pod:    podWithClaimName,
			claims: []*resourceapi.ResourceClaim{pendingClaim},
			oldObj: pendingClaim,
			newObj: func() *resourceapi.ResourceClaim {
				claim := pendingClaim.DeepCopy()
				claim.Status.Allocation = &resourceapi.AllocationResult{}
				return claim
			}(),
			expectedHint: framework.Queue,
		},
		"structured-claim-deallocate": {
			pod:    podWithClaimName,
			claims: []*resourceapi.ResourceClaim{pendingClaim, structuredClaim(otherAllocatedClaim)},
			oldObj: structuredClaim(otherAllocatedClaim),
			newObj: func() *resourceapi.ResourceClaim {
				claim := structuredClaim(otherAllocatedClaim).DeepCopy()
				claim.Status.Allocation = nil
				return claim
			}(),
			// TODO (https://github.com/kubernetes/kubernetes/issues/123697): don't wake up
			// claims not using structured parameters.
			expectedHint: framework.Queue,
		},
	}

	for name, tc := range testcases {
		t.Run(name, func(t *testing.T) {
			logger, tCtx := ktesting.NewTestContext(t)
			features := feature.Features{
				EnableDynamicResourceAllocation: true,
			}
			testCtx := setup(t, nil, tc.claims, nil, nil, nil, features)
			oldObj := tc.oldObj
			newObj := tc.newObj
			if claim, ok := tc.newObj.(*resourceapi.ResourceClaim); ok {
				// Add or update through the client and wait until the event is processed.
				claimKey := claim.Namespace + "/" + claim.Name
				if tc.oldObj == nil {
					// Some test claims already have it. Clear for create.
					createClaim := claim.DeepCopy()
					createClaim.UID = ""
					storedClaim, err := testCtx.client.ResourceV1alpha3().ResourceClaims(createClaim.Namespace).Create(tCtx, createClaim, metav1.CreateOptions{})
					require.NoError(t, err, "create claim")
					claim = storedClaim
				} else {
					cachedClaim, err := testCtx.claimAssumeCache.Get(claimKey)
					require.NoError(t, err, "retrieve old claim")
					updateClaim := claim.DeepCopy()
					// The test claim doesn't have those (generated dynamically), so copy them.
					updateClaim.UID = cachedClaim.(*resourceapi.ResourceClaim).UID
					updateClaim.ResourceVersion = cachedClaim.(*resourceapi.ResourceClaim).ResourceVersion

					storedClaim, err := testCtx.client.ResourceV1alpha3().ResourceClaims(updateClaim.Namespace).Update(tCtx, updateClaim, metav1.UpdateOptions{})
					require.NoError(t, err, "update claim")
					claim = storedClaim
				}

				// Eventually the assume cache will have it, too.
				require.EventuallyWithT(t, func(t *assert.CollectT) {
					cachedClaim, err := testCtx.claimAssumeCache.Get(claimKey)
					require.NoError(t, err, "retrieve claim")
					if cachedClaim.(*resourceapi.ResourceClaim).ResourceVersion != claim.ResourceVersion {
						t.Errorf("cached claim not updated yet")
					}
				}, time.Minute, time.Second, "claim assume cache must have new or updated claim")

				// This has the actual UID and ResourceVersion,
				// which is relevant for
				// isSchedulableAfterClaimChange.
				newObj = claim
			}
			actualHint, err := testCtx.p.isSchedulableAfterClaimChange(logger, tc.pod, oldObj, newObj)
			if tc.expectedErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			require.Equal(t, tc.expectedHint, actualHint)
		})
	}
}

func Test_isSchedulableAfterPodSchedulingContextChange(t *testing.T) {
	testcases := map[string]struct {
		pod            *v1.Pod
		schedulings    []*resourceapi.PodSchedulingContext
		claims         []*resourceapi.ResourceClaim
		oldObj, newObj interface{}
		expectedHint   framework.QueueingHint
		expectedErr    bool
	}{
		"skip-deleted": {
			pod:          podWithClaimTemplate,
			oldObj:       scheduling,
			expectedHint: framework.QueueSkip,
		},
		"skip-missed-deleted": {
			pod: podWithClaimTemplate,
			oldObj: cache.DeletedFinalStateUnknown{
				Obj: scheduling,
			},
			expectedHint: framework.QueueSkip,
		},
		"backoff-wrong-old-object": {
			pod:         podWithClaimTemplate,
			oldObj:      "not-a-scheduling-context",
			newObj:      scheduling,
			expectedErr: true,
		},
		"backoff-missed-wrong-old-object": {
			pod: podWithClaimTemplate,
			oldObj: cache.DeletedFinalStateUnknown{
				Obj: "not-a-scheduling-context",
			},
			newObj:      scheduling,
			expectedErr: true,
		},
		"skip-unrelated-object": {
			pod:    podWithClaimTemplate,
			claims: []*resourceapi.ResourceClaim{pendingClaim},
			newObj: func() *resourceapi.PodSchedulingContext {
				scheduling := scheduling.DeepCopy()
				scheduling.Name += "-foo"
				return scheduling
			}(),
			expectedHint: framework.QueueSkip,
		},
		"backoff-wrong-new-object": {
			pod:         podWithClaimTemplate,
			oldObj:      scheduling,
			newObj:      "not-a-scheduling-context",
			expectedErr: true,
		},
		"skip-missing-claim": {
			pod:          podWithClaimTemplate,
			oldObj:       scheduling,
			newObj:       schedulingInfo,
			expectedHint: framework.QueueSkip,
		},
		"skip-missing-infos": {
			pod:          podWithClaimTemplateInStatus,
			claims:       []*resourceapi.ResourceClaim{pendingClaim},
			oldObj:       scheduling,
			newObj:       scheduling,
			expectedHint: framework.QueueSkip,
		},
		"queue-new-infos": {
			pod:          podWithClaimTemplateInStatus,
			claims:       []*resourceapi.ResourceClaim{pendingClaim},
			oldObj:       scheduling,
			newObj:       schedulingInfo,
			expectedHint: framework.Queue,
		},
		"queue-bad-selected-node": {
			pod:    podWithClaimTemplateInStatus,
			claims: []*resourceapi.ResourceClaim{pendingClaim},
			oldObj: func() *resourceapi.PodSchedulingContext {
				scheduling := schedulingInfo.DeepCopy()
				scheduling.Spec.SelectedNode = workerNode.Name
				return scheduling
			}(),
			newObj: func() *resourceapi.PodSchedulingContext {
				scheduling := schedulingInfo.DeepCopy()
				scheduling.Spec.SelectedNode = workerNode.Name
				scheduling.Status.ResourceClaims[0].UnsuitableNodes = append(scheduling.Status.ResourceClaims[0].UnsuitableNodes, scheduling.Spec.SelectedNode)
				return scheduling
			}(),
			expectedHint: framework.Queue,
		},
		"skip-spec-changes": {
			pod:    podWithClaimTemplateInStatus,
			claims: []*resourceapi.ResourceClaim{pendingClaim},
			oldObj: schedulingInfo,
			newObj: func() *resourceapi.PodSchedulingContext {
				scheduling := schedulingInfo.DeepCopy()
				scheduling.Spec.SelectedNode = workerNode.Name
				return scheduling
			}(),
			expectedHint: framework.QueueSkip,
		},
		"backoff-other-changes": {
			pod:    podWithClaimTemplateInStatus,
			claims: []*resourceapi.ResourceClaim{pendingClaim},
			oldObj: schedulingInfo,
			newObj: func() *resourceapi.PodSchedulingContext {
				scheduling := schedulingInfo.DeepCopy()
				scheduling.Finalizers = append(scheduling.Finalizers, "foo")
				return scheduling
			}(),
			expectedHint: framework.Queue,
		},
	}

	for name, tc := range testcases {
		tc := tc
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			logger, _ := ktesting.NewTestContext(t)
			features := feature.Features{
				EnableDynamicResourceAllocation: true,
				EnableDRAControlPlaneController: true,
			}
			testCtx := setup(t, nil, tc.claims, nil, tc.schedulings, nil, features)
			actualHint, err := testCtx.p.isSchedulableAfterPodSchedulingContextChange(logger, tc.pod, tc.oldObj, tc.newObj)
			if tc.expectedErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			require.Equal(t, tc.expectedHint, actualHint)
		})
	}
}
