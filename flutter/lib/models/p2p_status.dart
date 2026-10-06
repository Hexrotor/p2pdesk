import 'dart:async';
import 'dart:convert';

import 'package:flutter/foundation.dart';
import 'package:get/get.dart';

import 'platform_model.dart';

/// The set of PeerIds with a live swarm connection to this node (green dot
/// in the peer lists), plus the app-level poll that keeps it fresh.
///
/// The poll must not live inside a page's State: the dots are shown on
/// several pages, so it starts once right after the host warmup and the
/// set stays live for the whole app lifetime.
final onlinePeers = Rx<Set<String>>(<String>{});

void startP2pStatusPolling() {
  Future<void> refresh() async {
    try {
      final j = jsonDecode(bind.p2PStatus()) as Map<String, dynamic>;
      final peers = (j['peers'] as String?) ?? '';
      final s = peers.split(',').where((p) => p.isNotEmpty).toSet();
      // Assign only when the set changed — the swarm connection set churns
      // (DHT maintenance connections), so polls that see the same set must
      // not rebuild the dot widgets.
      if (!setEquals(s, onlinePeers.value)) {
        onlinePeers.value = s;
      }
    } catch (_) {
      // host not initialized yet — keep the previous set
    }
  }

  refresh();
  Timer.periodic(const Duration(seconds: 5), (_) => refresh());
}

/// The active connect phase ("searching" / "dialing" / "handshake"), or null
/// when the controller is idle or the phase is stale. `phase_since` guards
/// against a stale phase lingering after a disconnect returned us to a page:
/// the connect budget is 70s (p2pd_connect total) plus an 18s handshake read
/// timeout, so anything older than 90s is stale.
String? activeConnectPhase(Map<String, dynamic> status) {
  final phase = status['phase'] as String? ?? 'idle';
  final phaseSince = (status['phase_since'] as num?)?.toInt() ?? 0;
  if (phaseSince <= 0) return null;
  final age = DateTime.now().millisecondsSinceEpoch - phaseSince;
  if (age > 90000) return null;
  return switch (phase) {
    'searching' || 'dialing' || 'handshake' => phase,
    _ => null,
  };
}
