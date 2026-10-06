#!/usr/bin/env bash

echo $MACOS_CODESIGN_IDENTITY
cargo install flutter_rust_bridge_codegen --version 1.80.1 --features uuid --locked
cd flutter; flutter pub get; cd -
~/.cargo/bin/flutter_rust_bridge_codegen --rust-input ./src/flutter_ffi.rs --dart-output ./flutter/lib/generated_bridge.dart --c-output ./flutter/macos/Runner/bridge_generated.h
./build.py --flutter
rm p2pdesk-$VERSION.dmg
# security find-identity -v
codesign --force --options runtime -s $MACOS_CODESIGN_IDENTITY --deep --strict ./flutter/build/macos/Build/Products/Release/P2PDesk.app -vvv
create-dmg --icon "P2PDesk.app" 200 190 --hide-extension "P2PDesk.app" --window-size 800 400 --app-drop-link 600 185 p2pdesk-$VERSION.dmg ./flutter/build/macos/Build/Products/Release/P2PDesk.app
codesign --force --options runtime -s $MACOS_CODESIGN_IDENTITY --deep --strict p2pdesk-$VERSION.dmg -vvv
# notarize the p2pdesk-${{ env.VERSION }}.dmg
rcodesign notary-submit --api-key-path ~/.p12/api-key.json  --staple p2pdesk-$VERSION.dmg
