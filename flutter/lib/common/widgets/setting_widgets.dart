import 'package:debounce_throttle/debounce_throttle.dart';
import 'package:flutter/material.dart';
import 'package:flutter_hbb/common.dart';
import 'package:flutter_hbb/consts.dart';
import 'package:flutter_hbb/models/platform_model.dart';
import 'package:get/get.dart';

customImageQualityWidget(
    {required double initQuality,
    required double initFps,
    required Function(double)? setQuality,
    required Function(double)? setFps,
    required bool showFps}) {
  if (initQuality < kMinQuality || initQuality > kMaxMoreQuality) {
    initQuality = kDefaultQuality;
  }
  final qualityValue = initQuality.obs;

  final debouncerQuality = Debouncer<double>(
    Duration(milliseconds: 1000),
    onChanged: setQuality,
    initialValue: qualityValue.value,
  );

  return Column(
    children: [
      Obx(() => Row(
            children: [
              Expanded(
                flex: 3,
                child: Slider(
                  value: qualityValue.value,
                  min: kMinQuality,
                  max: kMaxMoreQuality,
                  divisions: null,
                  onChanged: setQuality == null
                      ? null
                      : (double value) async {
                          qualityValue.value = value;
                          debouncerQuality.value = value;
                        },
                ),
              ),
              Expanded(
                  flex: 1,
                  child: Text(
                    '${qualityValue.value.round()} kbps',
                    style: const TextStyle(fontSize: 15),
                  )),
              Expanded(
                  flex: isMobile ? 2 : 1,
                  child: Text(
                    translate('Bitrate'),
                    style: const TextStyle(fontSize: 15),
                  )),
            ],
          )),
      if (showFps) customFrameRateWidget(initFps: initFps, setFps: setFps),
    ],
  );
}

Widget customImageQualitySetting() {
  final qualityKey = 'custom-bitrate-kbps';

  final initQuality =
      (double.tryParse(bind.mainGetUserDefaultOption(key: qualityKey)) ??
          kDefaultQuality);
  final isQuanlityFixed = isOptionFixed(qualityKey);
  return customImageQualityWidget(
      initQuality: initQuality,
      initFps: kDefaultFps,
      setQuality: isQuanlityFixed
          ? null
          : (v) async {
              await bind.mainSetUserDefaultOption(
                  key: qualityKey, value: v.round().toString());
            },
      setFps: null,
      showFps: false);
}

Widget customFrameRateWidget({
  required double initFps,
  required Function(double)? setFps,
}) {
  if (initFps < kMinFps || initFps > kMaxFps) initFps = kDefaultFps;
  final fpsValue = initFps.obs;
  final debouncerFps = Debouncer<double>(
    Duration(milliseconds: 1000),
    onChanged: setFps,
    initialValue: fpsValue.value,
  );
  return Obx(() => Row(
        children: [
          Expanded(
            flex: 3,
            child: Slider(
              value: fpsValue.value,
              min: kMinFps,
              max: kMaxFps,
              divisions: ((kMaxFps - kMinFps) / 5).round(),
              onChanged: setFps == null
                  ? null
                  : (double value) async {
                      fpsValue.value = value;
                      debouncerFps.value = value;
                    },
            ),
          ),
          Expanded(
              flex: 1,
              child: Text('${fpsValue.value.round()}',
                  style: const TextStyle(fontSize: 15))),
          Expanded(
              flex: 2,
              child:
                  Text(translate('FPS'), style: const TextStyle(fontSize: 15))),
        ],
      ));
}

Widget customFrameRateSetting() {
  const fpsKey = 'custom-fps';
  final initFps = double.tryParse(bind.mainGetUserDefaultOption(key: fpsKey)) ??
      kDefaultFps;
  final isFpsFixed = isOptionFixed(fpsKey);
  return customFrameRateWidget(
    initFps: initFps,
    setFps: isFpsFixed
        ? null
        : (v) => bind.mainSetUserDefaultOption(
            key: fpsKey, value: v.round().toString()),
  );
}

List<(String, String)> otherDefaultSettings() {
  List<(String, String)> v = [
    ('View Mode', kOptionViewOnly),
    if ((isDesktop || isWebDesktop))
      ('show_monitors_tip', kKeyShowMonitorsToolbar),
    if ((isDesktop || isWebDesktop))
      ('Collapse toolbar', kOptionCollapseToolbar),
    ('Show remote cursor', kOptionShowRemoteCursor),
    ('Follow remote cursor', kOptionFollowRemoteCursor),
    ('Follow remote window focus', kOptionFollowRemoteWindow),
    if ((isDesktop || isWebDesktop)) ('Zoom cursor', kOptionZoomCursor),
    ('Show quality monitor', kOptionShowQualityMonitor),
    ('Mute', kOptionDisableAudio),
    if (isDesktop) ('Enable file copy and paste', kOptionEnableFileCopyPaste),
    ('Disable clipboard', kOptionDisableClipboard),
    ('Lock after session end', kOptionLockAfterSessionEnd),
    ('Privacy mode', kOptionPrivacyMode),
    ('True color (4:4:4)', kOptionI444),
    ('Reverse mouse wheel', kKeyReverseMouseWheel),
    ('swap-left-right-mouse', kOptionSwapLeftRightMouse),
    if (isDesktop)
      (
        'Show displays as individual windows',
        kKeyShowDisplaysAsIndividualWindows
      ),
    if (isDesktop)
      (
        'Use all my displays for the remote session',
        kKeyUseAllMyDisplaysForTheRemoteSession
      ),
    ('Keep terminal sessions on disconnect', kOptionTerminalPersistent),
  ];

  return v;
}

class TrackpadSpeedWidget extends StatefulWidget {
  final SimpleWrapper<int> value;
  // If null, no debouncer will be applied.
  final Function(int)? onDebouncer;

  TrackpadSpeedWidget({Key? key, required this.value, this.onDebouncer});

  @override
  TrackpadSpeedWidgetState createState() => TrackpadSpeedWidgetState();
}

class TrackpadSpeedWidgetState extends State<TrackpadSpeedWidget> {
  final TextEditingController _controller = TextEditingController();
  late final Debouncer<int> debouncerSpeed;

  set value(int v) => widget.value.value = v;
  int get value => widget.value.value;

  void updateValue(int newValue) {
    setState(() {
      value = newValue.clamp(kMinTrackpadSpeed, kMaxTrackpadSpeed);
      // Scale the trackpad speed value to a percentage for display purposes.
      _controller.text = value.toString();
      if (widget.onDebouncer != null) {
        debouncerSpeed.setValue(value);
      }
    });
  }

  @override
  void initState() {
    super.initState();
    debouncerSpeed = Debouncer<int>(
      Duration(milliseconds: 1000),
      onChanged: widget.onDebouncer,
      initialValue: widget.value.value,
    );
  }

  @override
  Widget build(BuildContext context) {
    if (_controller.text.isEmpty) {
      _controller.text = value.toString();
    }
    return Row(
      children: [
        Expanded(
          flex: 3,
          child: Slider(
            value: value.toDouble(),
            min: kMinTrackpadSpeed.toDouble(),
            max: kMaxTrackpadSpeed.toDouble(),
            divisions: ((kMaxTrackpadSpeed - kMinTrackpadSpeed) / 10).round(),
            onChanged: (double v) => updateValue(v.round()),
          ),
        ),
        Expanded(
            flex: 1,
            child: Row(
              children: [
                SizedBox(
                  width: 56,
                  child: TextField(
                    controller: _controller,
                    keyboardType: TextInputType.number,
                    textAlign: TextAlign.center,
                    onSubmitted: (text) {
                      int? v = int.tryParse(text);
                      if (v != null) {
                        updateValue(v);
                      }
                    },
                    style: const TextStyle(fontSize: 13),
                    decoration: InputDecoration(
                      contentPadding:
                          EdgeInsets.symmetric(vertical: 8.0, horizontal: 12.0),
                    ),
                  ),
                ).marginOnly(right: 8.0),
                Text(
                  '%',
                  style: const TextStyle(fontSize: 15),
                )
              ],
            )),
      ],
    );
  }
}
