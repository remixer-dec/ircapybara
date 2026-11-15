#!/usr/bin/env sh
# Builds the Go client core (../mobile) into app/libs/ircapybara.aar.
#
# Needs: Go, the Android SDK with an NDK installed (ANDROID_HOME set), and a JDK.
# gomobile and gobind are installed at the versions pinned in ../go.mod.
set -eu

cd "$(dirname "$0")/.."

GOBIN="$(go env GOPATH)/bin"
export PATH="$GOBIN:$PATH"

go install golang.org/x/mobile/cmd/gomobile golang.org/x/mobile/cmd/gobind
gomobile init

mkdir -p android/app/libs
gomobile bind \
    -target=android \
    -androidapi 30 \
    -javapkg=io.ircapybara \
    -ldflags="-s -w" \
    -o android/app/libs/ircapybara.aar \
    ./mobile

echo "Wrote android/app/libs/ircapybara.aar"
