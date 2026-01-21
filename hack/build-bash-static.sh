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
set -o errexit
set -o nounset

# Debug-log input values.
declare -p TARGETARCH
declare -p BASH_STATIC_GIT_REF

# Dependencies for bash-static build.
apt-get install -y gpg curl autoconf file

git clone https://github.com/robxu9/bash-static/

# Copied unchanged from Dockerfile. Can of course be improved for readability.
ARCH="$TARGETARCH" && STRIP=strip && \
[ "$ARCH" = "arm64" ] && ARCH="aarch64" || true && \
[ "$ARCH" = "amd64" ] && ARCH="x86_64" || true && \
[ "$ARCH" = "aarch64" ] && STRIP="aarch64-linux-gnu-strip" && CC="aarch64-linux-gnu-gcc" || true && \
echo "detected arch: $ARCH" && \
echo "cc to use: $CC" && \
echo "strip to use: $STRIP" && \
cd bash-static && git checkout ${BASH_STATIC_GIT_REF} && \
sed -i 's|https://ftp\.gnu\.org/gnu|https://mirrors.kernel.org/gnu/|g' ./build.sh && \
sed -i 's/-sLO/-sSfLO --retry 300 --connect-timeout 20 --retry-delay 5 --retry-all-errors /g' ./build.sh && \
sed -i 's/strip/$STRIP/g' ./build.sh && \
sed -i 's/make -s \&\& make -s tests/make -j4/g' ./build.sh && \
bash version-52.sh && STRIP=$STRIP CC=$CC ./build.sh linux $ARCH
