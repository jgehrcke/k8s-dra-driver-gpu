#!/usr/bin/env bash

# Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

set -o pipefail

# if arg1 is set, it will be used as the version number
if [ -z "$1" ]; then
  VERSION=$(awk -F= '/^VERSION/ { print $2 }' versions.mk | tr -d '[:space:]')
else
  VERSION=$1
fi
# Remove any v prefix, if exists.
VERSION="${VERSION#v}"


# Note(JP): the goal below is for VERSION to always be
# strictly semver-compliant (parseable with a semver
# parser). That enables best compatibility with the Helm
# ecosystem. For example, that means that no `v` prefix
# should be used here.

# Create release assets to be uploaded
helm package deployments/helm/nvidia-dra-driver-gpu/ --version $VERSION --app-version $VERSION
