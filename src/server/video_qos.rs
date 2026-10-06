use super::*;
use scrap::codec::{Quality, BR_BALANCED, BR_BEST, BR_SPEED};
use std::{
    collections::VecDeque,
    time::{Duration, Instant},
};

/*
FPS adjust:
a. new user connected =>set to INIT_FPS
b. TestDelay receive => update user's fps according to network delay
    When network delay < DELAY_THRESHOLD_150MS, set minimum fps according to image quality, and increase fps;
    When network delay >= DELAY_THRESHOLD_150MS, set minimum fps according to image quality, and decrease fps;
c. second timeout / TestDelay receive => update real fps to the minimum fps from all users

ratio adjust:
a. user set image quality => update to the maximum ratio of the latest quality
b. 3 seconds timeout => update ratio according to network delay
    When network delay < DELAY_THRESHOLD_150MS, increase ratio, max 150kbps;
    When network delay >= DELAY_THRESHOLD_150MS, decrease ratio;

adjust between FPS and ratio:
    When network delay < DELAY_THRESHOLD_150MS, fps is always higher than the minimum fps, and ratio is increasing;
    When network delay >= DELAY_THRESHOLD_150MS, fps is always lower than the minimum fps, and ratio is decreasing;

delay:
    use delay minus RTT as the actual network delay
*/

// Constants
pub const FPS: u32 = 30;
pub const MIN_FPS: u32 = 1;
pub const MAX_FPS: u32 = 120;
pub const INIT_FPS: u32 = 15;

// Bitrate ratio constants for different quality levels
const BR_MAX: f32 = 40.0; // 2000 * 2 / 100
const BR_MIN: f32 = 0.2;
const BR_MIN_HIGH_RESOLUTION: f32 = 0.1; // For high resolution, BR_MIN is still too high, so we set a lower limit
const BR_MIN_CUSTOM: f32 = 0.04; // 2% slider value, about 16 KB/s at 2560x1600.
const MAX_BR_MULTIPLE: f32 = 1.0;

const HISTORY_DELAY_LEN: usize = 2;
const ADJUST_RATIO_INTERVAL: usize = 3; // Adjust quality ratio every 3 seconds
const DYNAMIC_SCREEN_THRESHOLD: usize = 2; // Allow increase quality ratio if encode more than 2 times in one second
const DELAY_THRESHOLD_150MS: u32 = 150; // 150ms is the threshold for good network condition

#[derive(Default, Debug, Clone)]
struct UserDelay {
    response_delayed: bool,
    delay_history: VecDeque<u32>,
    fps: Option<u32>,
    rtt_calculator: RttCalculator,
    quick_increase_fps_count: usize,
    increase_fps_count: usize,
}

impl UserDelay {
    fn add_delay(&mut self, delay: u32) {
        self.rtt_calculator.update(delay);
        if self.delay_history.len() > HISTORY_DELAY_LEN {
            self.delay_history.pop_front();
        }
        self.delay_history.push_back(delay);
    }

    // Average delay minus RTT
    fn avg_delay(&self) -> u32 {
        let len = self.delay_history.len();
        if len > 0 {
            let avg_delay = self.delay_history.iter().sum::<u32>() / len as u32;

            // Estimate queueing above the path's baseline RTT. Equal or lower
            // samples must not suddenly turn into a full RTT of queueing.
            if let Some(rtt) = self.rtt_calculator.get_rtt() {
                avg_delay.saturating_sub(rtt)
            } else {
                avg_delay
            }
        } else {
            DELAY_THRESHOLD_150MS
        }
    }
}

// User session data structure
#[derive(Default, Debug, Clone)]
struct UserData {
    auto_adjust_fps: Option<u32>, // receiver's runtime processing limit
    custom_fps: Option<u32>,
    quality: Option<(i64, Quality)>, // (time, quality)
    delay: UserDelay,
    record: bool,
}

#[derive(Default, Debug, Clone)]
struct DisplayData {
    send_counter: usize, // Number of times encode during period
    support_changing_quality: bool,
}

// Main QoS controller structure
pub struct VideoQoS {
    fps: u32,
    ratio: f32,
    users: HashMap<i32, UserData>,
    displays: HashMap<String, DisplayData>,
    bitrate_store: u32,
    adjust_ratio_instant: Instant,
    abr_config: bool,
    new_user_instant: Instant,
    quality_recovery_streak: u8,
    // Ceiling for the adaptive frame rate, set from measured pipeline stage
    // costs; MAX_FPS means "no measured bound yet".
    sender_fps_cap: u32,
}

impl Default for VideoQoS {
    fn default() -> Self {
        VideoQoS {
            fps: FPS,
            ratio: BR_BALANCED,
            users: Default::default(),
            displays: Default::default(),
            bitrate_store: 0,
            adjust_ratio_instant: Instant::now(),
            abr_config: true,
            new_user_instant: Instant::now(),
            quality_recovery_streak: 0,
            sender_fps_cap: MAX_FPS,
        }
    }
}

// Basic functionality
impl VideoQoS {
    pub(super) fn diagnostic(&self) -> String {
        let users: Vec<_> = self
            .users
            .iter()
            .map(|(id, u)| {
                (
                    *id,
                    u.custom_fps.unwrap_or(FPS),
                    u.auto_adjust_fps,
                    u.delay.fps,
                    u.delay.rtt_calculator.get_rtt(),
                    u.delay.avg_delay(),
                    u.delay.response_delayed,
                )
            })
            .collect();
        format!("qos_fps={} cap={} ratio={:.3} abr={} users(id,requested,receiver,network,rtt_ms,queue_ms,delayed)={users:?}",
            self.fps(), self.highest_fps(), self.ratio, self.in_vbr_state())
    }

    // Calculate seconds per frame based on current FPS
    pub fn spf(&self) -> Duration {
        Duration::from_secs_f32(1. / (self.fps() as f32))
    }

    // Get current FPS within valid range
    pub fn fps(&self) -> u32 {
        let fps = self.fps;
        if fps >= MIN_FPS && fps <= MAX_FPS {
            fps
        } else {
            FPS
        }
    }

    // Store bitrate for later use
    pub fn store_bitrate(&mut self, bitrate: u32) {
        self.bitrate_store = bitrate;
    }

    // Get stored bitrate
    pub fn bitrate(&self) -> u32 {
        self.bitrate_store
    }

    // Get current bitrate ratio with bounds checking
    pub fn ratio(&mut self) -> f32 {
        if self.ratio < BR_MIN_CUSTOM || self.ratio > BR_MAX {
            self.ratio = BR_BALANCED;
        }
        self.ratio
    }

    // Check if any user is in recording mode
    pub fn record(&self) -> bool {
        self.users.iter().any(|u| u.1.record)
    }

    pub fn set_support_changing_quality(&mut self, video_service_name: &str, support: bool) {
        if let Some(display) = self.displays.get_mut(video_service_name) {
            display.support_changing_quality = support;
        }
    }

    // Check if variable bitrate encoding is supported and enabled
    pub fn in_vbr_state(&self) -> bool {
        self.abr_config && self.displays.iter().all(|e| e.1.support_changing_quality)
    }
}

// User session management
impl VideoQoS {
    // Initialize new user session
    pub fn on_connection_open(&mut self, id: i32) {
        self.users.insert(id, UserData::default());
        self.abr_config = Config::get_option("enable-abr") != "N";
        self.new_user_instant = Instant::now();
    }

    // Clean up user session
    pub fn on_connection_close(&mut self, id: i32) {
        self.users.remove(&id);
        if self.users.is_empty() {
            *self = Default::default();
        }
    }

    pub fn user_custom_fps(&mut self, id: i32, fps: u32) {
        if fps < MIN_FPS || fps > MAX_FPS {
            return;
        }
        if let Some(user) = self.users.get_mut(&id) {
            user.custom_fps = Some(fps);
        }
        self.adjust_fps();
    }

    pub fn user_auto_adjust_fps(&mut self, id: i32, fps: u32) {
        if fps < MIN_FPS || fps > MAX_FPS {
            return;
        }
        if let Some(user) = self.users.get_mut(&id) {
            user.auto_adjust_fps = Some(fps);
        }
        self.adjust_fps();
    }

    pub fn user_image_quality(&mut self, id: i32, image_quality: i32) {
        let convert_quality = |q: i32| -> Quality {
            if q == ImageQuality::Balanced.value() {
                Quality::Balanced
            } else if q == ImageQuality::Low.value() {
                Quality::Low
            } else if q == ImageQuality::Best.value() {
                Quality::Best
            } else {
                let b = ((q >> 8 & 0xFFF) * 2) as f32 / 100.0;
                Quality::Custom(b.clamp(BR_MIN_CUSTOM, BR_MAX))
            }
        };

        let quality = Some((hbb_common::get_time(), convert_quality(image_quality)));
        if let Some(user) = self.users.get_mut(&id) {
            user.quality = quality;
            // update ratio directly
            self.ratio = self.latest_quality().ratio();
            self.quality_recovery_streak = 0;
        }
    }

    pub fn user_record(&mut self, id: i32, v: bool) {
        if let Some(user) = self.users.get_mut(&id) {
            user.record = v;
        }
    }

    pub fn user_network_delay(&mut self, id: i32, delay: u32) {
        let highest_fps = self.highest_fps();
        let target_ratio = self.latest_quality().ratio();

        // For bad network, small fps means quick reaction and high quality
        let (min_fps, normal_fps) = if target_ratio >= BR_BEST {
            (8, 16)
        } else if target_ratio >= BR_BALANCED {
            (10, 20)
        } else {
            (12, 24)
        };

        // Calculate minimum acceptable delay-fps product
        let dividend_ms = DELAY_THRESHOLD_150MS * min_fps;

        let mut adjust_ratio = false;
        if let Some(user) = self.users.get_mut(&id) {
            user.delay.response_delayed = false;
            let delay = delay.max(10);
            let old_avg_delay = user.delay.avg_delay();
            user.delay.add_delay(delay);
            let mut avg_delay = user.delay.avg_delay();
            avg_delay = avg_delay.max(10);
            let mut fps = self.fps;

            // Adaptive FPS adjustment based on network delay:
            if avg_delay < 50 {
                user.delay.quick_increase_fps_count += 1;
                let mut step = if fps < normal_fps { 1 } else { 0 };
                if user.delay.quick_increase_fps_count >= 3 {
                    // Recover proportionally towards the negotiated ceiling;
                    // adding only five frames makes high-fps sessions take minutes.
                    user.delay.quick_increase_fps_count = 0;
                    step = (fps / 2).max(5);
                }
                fps = min_fps.max(fps + step);
            } else if avg_delay < 100 {
                let step = if avg_delay < old_avg_delay {
                    if fps < normal_fps {
                        1
                    } else {
                        0
                    }
                } else {
                    0
                };
                fps = min_fps.max(fps + step);
            } else if avg_delay < DELAY_THRESHOLD_150MS {
                fps = min_fps.max(fps);
            } else {
                let devide_fps = ((fps as f32) / (avg_delay as f32 / DELAY_THRESHOLD_150MS as f32))
                    .ceil() as u32;
                if avg_delay < 200 {
                    fps = min_fps.max(devide_fps);
                } else if avg_delay < 300 {
                    fps = min_fps.min(devide_fps);
                } else if avg_delay < 600 {
                    fps = dividend_ms / avg_delay;
                } else {
                    fps = (dividend_ms / avg_delay).min(devide_fps);
                }
            }

            if avg_delay < DELAY_THRESHOLD_150MS {
                user.delay.increase_fps_count += 1;
            } else {
                user.delay.increase_fps_count = 0;
            }
            if user.delay.increase_fps_count >= 3 {
                // After 3 stable samples, try increasing FPS
                user.delay.increase_fps_count = 0;
                fps += 1;
            }

            // Reset quick increase counter if network condition worsens
            if avg_delay >= 50 {
                user.delay.quick_increase_fps_count = 0;
            }

            fps = fps.clamp(MIN_FPS, highest_fps);
            // first network delay message
            adjust_ratio = user.delay.fps.is_none();
            user.delay.fps = Some(fps);
        }
        self.adjust_fps();
        if adjust_ratio && !cfg!(target_os = "linux") {
            //Reduce the possibility of vaapi being created twice
            self.adjust_ratio(false);
        }
    }

    pub fn user_delay_response_elapsed(&mut self, id: i32, elapsed: u128) {
        if let Some(user) = self.users.get_mut(&id) {
            user.delay.response_delayed = elapsed > 2000;
            if user.delay.response_delayed {
                user.delay.add_delay(elapsed as u32);
                self.adjust_fps();
            }
        }
    }
}

// Common adjust functions
impl VideoQoS {
    pub fn new_display(&mut self, video_service_name: String) {
        self.displays
            .insert(video_service_name, DisplayData::default());
    }

    pub fn remove_display(&mut self, video_service_name: &str) {
        self.displays.remove(video_service_name);
    }

    pub fn update_display_data(&mut self, video_service_name: &str, send_counter: usize) {
        if let Some(display) = self.displays.get_mut(video_service_name) {
            display.send_counter += send_counter;
        }
        self.adjust_fps();
        let abr_enabled = self.in_vbr_state();
        if abr_enabled {
            if self.adjust_ratio_instant.elapsed().as_secs() >= ADJUST_RATIO_INTERVAL as u64 {
                let dynamic_screen = self
                    .displays
                    .iter()
                    .any(|d| d.1.send_counter >= ADJUST_RATIO_INTERVAL * DYNAMIC_SCREEN_THRESHOLD);
                self.displays.iter_mut().for_each(|d| {
                    d.1.send_counter = 0;
                });
                self.adjust_ratio(dynamic_screen);
            }
        } else {
            self.ratio = self.latest_quality().ratio();
            self.quality_recovery_streak = 0;
        }
    }

    /// Bound the adaptive frame rate by what the capture/convert/encode
    /// pipeline can actually sustain, so the encoder's per-frame budget stays
    /// reachable instead of being judged against a rate the machine cannot
    /// deliver.
    pub fn set_sender_fps_cap(&mut self, fps: u32) {
        self.sender_fps_cap = fps.clamp(MIN_FPS, MAX_FPS);
    }

    #[inline]
    fn highest_fps(&self) -> u32 {
        let user_fps = |u: &UserData| {
            let mut fps = u.custom_fps.unwrap_or(FPS);
            if let Some(auto_adjust_fps) = u.auto_adjust_fps {
                if fps == 0 || auto_adjust_fps < fps {
                    fps = auto_adjust_fps;
                }
            }
            fps
        };

        let fps = self
            .users
            .iter()
            .map(|(_, u)| user_fps(u))
            .filter(|u| *u >= MIN_FPS)
            .min()
            .unwrap_or(FPS)
            .min(self.sender_fps_cap);

        fps.clamp(MIN_FPS, MAX_FPS)
    }

    // Get latest quality settings from all users
    pub fn latest_quality(&self) -> Quality {
        self.users
            .iter()
            .map(|(_, u)| u.quality)
            .filter(|q| *q != None)
            .max_by(|a, b| a.unwrap_or_default().0.cmp(&b.unwrap_or_default().0))
            .flatten()
            .unwrap_or((0, Quality::Balanced))
            .1
    }

    // Adjust quality ratio based on network delay and screen changes
    fn adjust_ratio(&mut self, dynamic_screen: bool) {
        if !self.in_vbr_state() {
            return;
        }
        // Get maximum delay from all users
        let max_delay = self.users.iter().map(|u| u.1.delay.avg_delay()).max();
        let Some(max_delay) = max_delay else {
            self.quality_recovery_streak = 0;
            return;
        };

        // Require two consecutive good, active adjustment windows before raising
        // quality. Congestion still reduces it immediately; idle is not recovery.
        self.quality_recovery_streak = if dynamic_screen && max_delay < DELAY_THRESHOLD_150MS {
            self.quality_recovery_streak.saturating_add(1)
        } else {
            0
        };
        let recover_quality = self.quality_recovery_streak >= 2;

        let target_quality = self.latest_quality();
        let target_ratio = self.latest_quality().ratio();
        let current_ratio = self.ratio;
        let current_bitrate = self.bitrate();

        // Calculate minimum ratio for high resolution (1Mbps baseline)
        let ratio_1mbps = if current_bitrate > 0 {
            Some((current_ratio * 1000.0 / current_bitrate as f32).max(BR_MIN_HIGH_RESOLUTION))
        } else {
            None
        };

        // Calculate ratio for adding 150kbps bandwidth
        let ratio_add_150kbps = if current_bitrate > 0 {
            Some((current_bitrate + 150) as f32 * current_ratio / current_bitrate as f32)
        } else {
            None
        };

        // Set minimum ratio based on quality mode
        let min = match target_quality {
            Quality::Best => {
                // For Best quality, ensure minimum 1Mbps for high resolution
                let mut min = BR_BEST / 2.5;
                if let Some(ratio_1mbps) = ratio_1mbps {
                    if min > ratio_1mbps {
                        min = ratio_1mbps;
                    }
                }
                min.max(BR_MIN)
            }
            Quality::Balanced => {
                let mut min = (BR_BALANCED / 2.0).min(0.4);
                if let Some(ratio_1mbps) = ratio_1mbps {
                    if min > ratio_1mbps {
                        min = ratio_1mbps;
                    }
                }
                min.max(BR_MIN_HIGH_RESOLUTION)
            }
            Quality::Low => BR_MIN_HIGH_RESOLUTION,
            Quality::Custom(_) => BR_MIN_CUSTOM,
        };
        let max = target_ratio * MAX_BR_MULTIPLE;

        let mut v = current_ratio;

        // Adjust ratio based on network delay thresholds
        if max_delay < 50 {
            if recover_quality {
                v = current_ratio * 1.15;
            }
        } else if max_delay < 100 {
            if recover_quality {
                v = current_ratio * 1.1;
            }
        } else if max_delay < DELAY_THRESHOLD_150MS {
            if recover_quality {
                v = current_ratio * 1.05;
            }
        } else if max_delay < 200 {
            v = current_ratio * 0.95;
        } else if max_delay < 300 {
            v = current_ratio * 0.9;
        } else if max_delay < 500 {
            v = current_ratio * 0.85;
        } else {
            v = current_ratio * 0.8;
        }

        // Limit quality increase rate for better stability
        if let Some(ratio_add_150kbps) = ratio_add_150kbps {
            if v > ratio_add_150kbps
                && ratio_add_150kbps > current_ratio
                && current_ratio >= BR_SPEED
            {
                v = ratio_add_150kbps;
            }
        }

        self.ratio = v.clamp(min, max);
        self.adjust_ratio_instant = Instant::now();
    }

    // Adjust fps based on network delay and user response time
    fn adjust_fps(&mut self) {
        let highest_fps = self.highest_fps();
        // Get minimum fps from all users
        let mut fps = self
            .users
            .iter()
            .map(|u| u.1.delay.fps.unwrap_or(INIT_FPS))
            .min()
            .unwrap_or(INIT_FPS);

        if self.users.iter().any(|u| u.1.delay.response_delayed) {
            if fps > MIN_FPS + 1 {
                fps = MIN_FPS + 1;
            }
        }

        // For new connections (within 1 second), cap fps to INIT_FPS to ensure stability
        if self.new_user_instant.elapsed().as_secs() < 1 {
            if fps > INIT_FPS {
                fps = INIT_FPS;
            }
        }

        // Ensure fps stays within valid range
        self.fps = fps.clamp(MIN_FPS, highest_fps);
    }
}

#[derive(Default, Debug, Clone)]
struct RttCalculator {
    min_rtt: Option<u32>,        // Historical minimum RTT ever observed
    window_min_rtt: Option<u32>, // Minimum RTT within last 60 samples
    smoothed_rtt: Option<u32>,   // Smoothed RTT estimation
    samples: VecDeque<u32>,      // Last 60 RTT samples
}

impl RttCalculator {
    const WINDOW_SAMPLES: usize = 60; // Keep last 60 samples
    const MIN_SAMPLES: usize = 10; // Require at least 10 samples
    const ALPHA: f32 = 0.5; // Smoothing factor for weighted average

    /// Update RTT estimates with a new sample
    pub fn update(&mut self, delay: u32) {
        // 1. Update historical minimum RTT
        match self.min_rtt {
            Some(min_rtt) if delay < min_rtt => self.min_rtt = Some(delay),
            None => self.min_rtt = Some(delay),
            _ => {}
        }

        // 2. Update sample window
        if self.samples.len() >= Self::WINDOW_SAMPLES {
            self.samples.pop_front();
        }
        self.samples.push_back(delay);

        // 3. Calculate minimum RTT within the window
        self.window_min_rtt = self.samples.iter().min().copied();

        // 4. Calculate smoothed RTT
        // Use weighted average if we have enough samples
        if self.samples.len() >= Self::WINDOW_SAMPLES {
            if let (Some(min), Some(window_min)) = (self.min_rtt, self.window_min_rtt) {
                // Weighted average of historical minimum and window minimum
                let new_srtt =
                    ((1.0 - Self::ALPHA) * min as f32 + Self::ALPHA * window_min as f32) as u32;
                self.smoothed_rtt = Some(new_srtt);
            }
        }
    }

    /// Get current RTT estimate
    /// Returns None if no valid estimation is available
    pub fn get_rtt(&self) -> Option<u32> {
        if let Some(rtt) = self.smoothed_rtt {
            return Some(rtt);
        }
        if self.samples.len() >= Self::MIN_SAMPLES {
            if let Some(rtt) = self.min_rtt {
                return Some(rtt);
            }
        }
        None
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn quality_recovery_requires_sustained_active_feedback() {
        let mut qos = VideoQoS {
            ratio: 0.5,
            ..Default::default()
        };
        qos.users.insert(1, UserData::default());
        // Use single samples with no RTT estimator to specify queueing directly.
        let feedback = |qos: &mut VideoQoS, delay, active| {
            let user = qos.users.get_mut(&1).unwrap();
            user.delay.delay_history = VecDeque::from([delay]);
            qos.adjust_ratio(active);
        };
        feedback(&mut qos, 600, true);
        let reduced = qos.ratio;
        assert!(reduced < 0.5);
        feedback(&mut qos, 20, true);
        assert_eq!(
            qos.ratio, reduced,
            "one good window must not bounce quality"
        );
        feedback(&mut qos, 20, false);
        feedback(&mut qos, 20, true);
        assert_eq!(qos.ratio, reduced, "idle must reset recovery evidence");
        feedback(&mut qos, 20, true);
        assert!(qos.ratio > reduced);
        for _ in 0..20 {
            feedback(&mut qos, 20, true);
        }
        assert!(qos.ratio <= qos.latest_quality().ratio());
        feedback(&mut qos, 600, true);
        let reduced = qos.ratio;
        feedback(&mut qos, 20, true);
        assert_eq!(qos.ratio, reduced);
        qos.abr_config = false;
        qos.update_display_data("test", 60);
        assert_eq!(qos.ratio, qos.latest_quality().ratio());
        assert_eq!(qos.quality_recovery_streak, 0);
    }

    // Deterministic control-loop experiment; does not initialize a server or write config.
    #[test]
    fn requested_120_fps_response_to_delay() {
        for delay in [20, 80, 160] {
            let mut qos = VideoQoS {
                new_user_instant: Instant::now() - Duration::from_secs(2),
                ..Default::default()
            };
            qos.users.insert(1, UserData::default());
            qos.user_custom_fps(1, 120);
            qos.adjust_fps();
            assert_eq!(qos.highest_fps(), 120);
            for _ in 1..=360 {
                qos.user_network_delay(1, delay);
                assert!(qos.fps() <= 120);
            }
            if delay == 20 {
                assert_eq!(qos.fps(), 120);
            }
            qos.user_custom_fps(1, 24);
            qos.adjust_fps();
            assert_eq!(qos.highest_fps(), 24);
            assert!(qos.fps() <= 24);
        }
    }

    #[test]
    fn stable_rtt_equal_to_mean_has_no_queue_delay() {
        let mut delay = UserDelay::default();
        for _ in 0..60 {
            delay.add_delay(80);
        }
        assert_eq!(delay.rtt_calculator.get_rtt(), Some(80));
        assert_eq!(delay.avg_delay(), 0);
    }

    #[test]
    fn requested_fps_respects_receiver_and_shared_session_caps() {
        let mut qos = VideoQoS {
            new_user_instant: Instant::now() - Duration::from_secs(2),
            ..Default::default()
        };
        qos.users.insert(1, UserData::default());
        qos.user_custom_fps(1, 120);
        qos.user_auto_adjust_fps(1, 60);
        for _ in 0..90 {
            qos.user_network_delay(1, 20);
            assert!(qos.fps() <= 60);
        }
        assert_eq!(qos.fps(), 60);
        qos.users.insert(2, UserData::default());
        qos.user_custom_fps(2, 30);
        for _ in 0..90 {
            qos.user_network_delay(1, 20);
            qos.user_network_delay(2, 20);
            assert!(qos.fps() <= 30);
        }
        assert_eq!(qos.fps(), 30);
        qos.on_connection_close(2);
        for _ in 0..90 {
            qos.user_network_delay(1, 20);
        }
        assert_eq!(qos.fps(), 60);
    }

    #[test]
    fn stable_mobile_rtt_recovers_high_fps_without_overwriting_user_target() {
        let mut qos = VideoQoS {
            new_user_instant: Instant::now() - Duration::from_secs(2),
            ..Default::default()
        };
        qos.users.insert(1, UserData::default());
        qos.user_custom_fps(1, 120);
        qos.user_auto_adjust_fps(1, 60);
        for _ in 0..30 {
            qos.user_network_delay(1, 160);
        }
        assert_eq!(qos.fps(), 60);
        assert_eq!(qos.users[&1].custom_fps, Some(120));
        // A recovered receiver raises only its own limit, retaining the user's 120 target.
        qos.user_auto_adjust_fps(1, 120);
        for _ in 0..15 {
            qos.user_network_delay(1, 160);
        }
        assert_eq!(qos.fps(), 120);
        // Queueing above the established path RTT must still reduce output rate.
        for _ in 0..10 {
            qos.user_network_delay(1, 600);
        }
        assert!(qos.fps() < 60);
        assert_eq!(qos.users[&1].custom_fps, Some(120));
    }

    #[test]
    fn custom_quality_allows_two_percent_low_traffic_target() {
        let mut qos = VideoQoS::default();
        qos.users.insert(1, UserData::default());
        qos.user_image_quality(1, 2 << 8);
        assert!((qos.ratio() - BR_MIN_CUSTOM).abs() < f32::EPSILON);
    }
}
