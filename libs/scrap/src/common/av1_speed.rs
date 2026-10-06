//! Select a libaom realtime speed without changing bitrate, quantizers or frame rate.
use std::time::{Duration, Instant};

pub(super) struct SpeedController {
    pub(super) speed: u32,
    budget: Option<Duration>,
    samples: Vec<Duration>,
    last_frame: Option<Instant>,
    last_change: Option<Instant>,
    overload: u8,
    disabled: bool,
}

impl SpeedController {
    pub(super) fn new(speed: u32) -> Self {
        Self {
            speed,
            budget: None,
            samples: Vec::with_capacity(30),
            last_frame: None,
            last_change: None,
            overload: 0,
            disabled: false,
        }
    }

    pub(super) fn set_fps(&mut self, fps: u32) {
        let budget = Duration::from_secs_f64(1.0 / fps.clamp(1, 120) as f64);
        if self.budget != Some(budget) {
            self.budget = Some(budget);
            // Durations stay comparable across budget changes, so the p95
            // window survives and the next step-down does not start from zero.
            // Restart the streak (it is budget-relative) and re-arm the
            // cooldown so the first decision rests on fresh evidence only.
            self.overload = 0;
            self.last_change = Some(Instant::now());
        }
    }

    pub(super) fn budget_ms(&self) -> Option<f64> {
        self.budget.map(|budget| budget.as_secs_f64() * 1000.0)
    }

    pub(super) fn overload(&self) -> u8 {
        self.overload
    }

    #[cfg(test)]
    pub(super) fn samples_len(&self) -> usize {
        self.samples.len()
    }

    pub(super) fn disable(&mut self) {
        self.disabled = true;
        self.samples.clear();
    }

    // Proposals are committed only after libaom accepts the control change.
    pub(super) fn observe(&mut self, now: Instant, elapsed: Duration, key: bool) -> Option<u32> {
        let budget = self.budget?;
        if self.disabled {
            return None;
        }
        let idle = self
            .last_frame
            .map(|last| {
                now.saturating_duration_since(last) > Duration::from_millis(500).max(budget * 5)
            })
            .unwrap_or(true);
        self.last_frame = Some(now);
        if idle || key {
            self.samples.clear();
            self.overload = 0;
            self.last_change = Some(now);
            return None;
        }
        let last_change = *self.last_change.get_or_insert(now);
        let load = elapsed.as_secs_f64() / budget.as_secs_f64();
        self.overload = if load > 0.9 {
            self.overload.saturating_add(1)
        } else {
            0
        };
        // Three consecutive frames above 90% of the budget, not a single
        // spike: one slow frame (scheduler hiccup, scene cut) must not drop
        // quality for seconds.
        if self.overload >= 3 && self.speed < 10 {
            return Some(10);
        }
        self.samples.push(elapsed);
        if self.samples.len() < 30 {
            return None;
        }
        self.samples.sort_unstable();
        let p95 = self.samples[28].as_secs_f64() / budget.as_secs_f64();
        self.samples.clear();
        if self.speed > 8
            && p95 < 0.6
            && now.saturating_duration_since(last_change) >= Duration::from_secs(2)
        {
            Some(self.speed - 1)
        } else {
            None
        }
    }

    pub(super) fn commit(&mut self, speed: u32, now: Instant) {
        self.speed = speed;
        self.last_change = Some(now);
        self.samples.clear();
        self.overload = 0;
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn feed(c: &mut SpeedController, now: &mut Instant, count: usize, elapsed_ms: u64) {
        for _ in 0..count {
            *now += Duration::from_millis(34);
            if let Some(speed) = c.observe(*now, Duration::from_millis(elapsed_ms), false) {
                c.commit(speed, *now);
            }
        }
    }

    #[test]
    fn a_single_slow_frame_keeps_the_speed_but_sustained_overload_snaps_back() {
        let mut c = SpeedController::new(8);
        c.set_fps(30);
        let mut now = Instant::now();
        feed(&mut c, &mut now, 30, 5);
        assert_eq!(c.speed, 8);
        now += Duration::from_millis(34);
        assert_eq!(
            c.observe(now, Duration::from_millis(200), false),
            None,
            "one frame far over the budget must not drop quality"
        );
        assert_eq!(c.speed, 8);
        feed(&mut c, &mut now, 3, 200);
        assert_eq!(c.speed, 10, "sustained overload still snaps back");
    }

    #[test]
    fn budget_changes_keep_the_sample_window() {
        let mut c = SpeedController::new(9);
        c.set_fps(30);
        let mut now = Instant::now();
        feed(&mut c, &mut now, 10, 5);
        assert!(c.samples_len() > 0);
        c.set_fps(120);
        assert!(
            c.samples_len() > 0,
            "a budget change must not discard the evidence window"
        );
        assert_eq!(c.overload(), 0, "the overload streak is budget-relative");
    }

    #[test]
    fn spends_headroom_gradually_and_recovers_from_cpu_pressure() {
        let mut c = SpeedController::new(10);
        c.set_fps(30);
        let mut now = Instant::now();
        feed(&mut c, &mut now, 30, 5);
        assert_eq!(c.speed, 10);
        feed(&mut c, &mut now, 180, 5);
        assert_eq!(c.speed, 8);
        feed(&mut c, &mut now, 3, 32);
        assert_eq!(c.speed, 10);
        feed(&mut c, &mut now, 30, 5);
        assert_eq!(c.speed, 10, "must hold fast speed after overload");
    }

    #[test]
    fn ignores_idle_time_and_keyframe_spikes_and_respects_tighter_budget() {
        let mut c = SpeedController::new(9);
        c.set_fps(30);
        let mut now = Instant::now();
        feed(&mut c, &mut now, 50, 5);
        now += Duration::from_secs(5);
        assert_eq!(c.observe(now, Duration::from_millis(100), true), None);
        feed(&mut c, &mut now, 30, 5);
        assert_eq!(c.speed, 9, "idle time must not count as encoding headroom");
        c.set_fps(120);
        feed(&mut c, &mut now, 4, 10);
        assert_eq!(c.speed, 10);
        c.disable();
        c.set_fps(30);
        feed(&mut c, &mut now, 300, 1);
        assert_eq!(
            c.speed, 10,
            "failed controls stay disabled for this encoder"
        );
    }
}
