/*
Copyright 2026 mohammedamrath.

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

package v1

const (
	// LabelNodePool is the key a Pod uses (via nodeSelector or required node
	// affinity) to request a pool, and that nodes provisioned by that pool carry
	// so the scheduler can bind the Pod once the node registers.
	LabelNodePool = "xenonite.io/nodepool"

	// LabelNodeClaim is set on a provisioned Node to link it back to its claim.
	LabelNodeClaim = "xenonite.io/nodeclaim"

	// FinalizerNodeClaim guards a XenonNodeClaim so the backing VM is deleted
	// before the object is removed from the API server.
	FinalizerNodeClaim = "xenonite.io/nodeclaim"
)
