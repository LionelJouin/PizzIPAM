#!/bin/bash

# Copyright 2026 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Validates the CEL in ./deployment the way the API server does at apply time,
# without a cluster: CRD x-kubernetes-validations cost budget + compilation, and
# ValidatingAdmissionPolicy / MutatingAdmissionPolicy expression compilation.
# This catches the "estimated rule cost exceeds budget" apply failure and CEL
# typos in the policies before they ever reach a cluster.

set -o errexit
set -o nounset
set -o pipefail

cd "$(git rev-parse --show-toplevel)" || exit 1

DEPLOYMENT="$(pwd)/deployment"

# celcheck is a separate Go module (hack/celcheck) so the apiserver validator
# deps stay out of the root module. Run it from there against ./deployment.
cd hack/celcheck
go run . "${DEPLOYMENT}"
