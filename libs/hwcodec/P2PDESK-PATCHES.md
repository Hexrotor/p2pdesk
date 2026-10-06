# Local hwcodec build integration

Source: https://github.com/rustdesk-org/hwcodec at 778df1f9 (version 0.7.1).
Upstream publishes no license file; the code is redistributed here unmodified
except for the patches below, as part of this rustdesk-derived codebase that is
released under AGPL-3.0 (see the repository NOTICE). This dependency is selected
through the root Cargo patch table so no Cargo cache edits are required.
The upstream `externals` submodule is retained at commit
`8903740a1f47884906a6e347ad3d8d56304d9771`; initialize repository submodules
when checking out a clean tree.

Local changes:

- Select platform C++ sources and SDK linkage using `CARGO_CFG_TARGET_OS`.
  A Windows build host must not compile Direct3D sources into an Android library.
- Link the Android C++ shared runtime already shipped with the APK; package the
  matching NDK runtime alongside the native libraries.
- Retain the existing cache's arm64 correction: link Intel libmfx only on
  Windows x86/x64, matching this project's FFmpeg configuration.
- Open MediaCodec decoders when the first Annex-B keyframe supplies SPS/PPS/VPS
  and dimensions; obtain display/coded dimensions from the FFmpeg parser.
- Treat accepted input with no output yet (EAGAIN) as normal asynchronous
  decoder progress rather than a codec failure.

Android uses the existing FFmpeg RAM codec path. The separate experimental
`mediacodec` Cargo feature is not enabled by the release script.
