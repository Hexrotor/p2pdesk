// Capture timing probe. --motion draws a temporary test window; no input injection.
#[cfg(windows)]
fn main() -> Result<(), Box<dyn std::error::Error>> {
    use scrap::{Capturer, Display, Frame, TraitCapturer, TraitPixelBuffer};
    use std::time::{Duration, Instant};
    if unsafe { winapi::um::winuser::SetProcessDPIAware() } == 0 {
        return Err(std::io::Error::last_os_error().into());
    }
    let display = Display::all()?.into_iter().find(|d| d.is_primary())
        .ok_or("No primary display")?;
    println!("CAPTURE_PROBE display={} size={}x{}", display.name(), display.width(), display.height());
    let mut capturer = Capturer::new(display)?;
    if std::env::args().any(|arg| arg == "--gdi") && !capturer.set_gdi() {
        return Err("Failed to select GDI".into());
    }
    println!("CAPTURE_PROBE initial_gdi={}", capturer.is_gdi());
    let motion = std::env::args().any(|arg| arg == "--motion");
    let window = if motion { Some(MotionWindow::new()?) } else { None };
    let start = Instant::now();
    let mut count = 0;
    let mut idle = 0;
    let mut total = Duration::ZERO;
    let mut first_ms = None;
    let mut verified = 0;
    let mut changes = 0;
    let mut last_position = None;
    while start.elapsed() < Duration::from_secs(5) {
        if let Some(window) = &window { window.draw(start.elapsed().as_millis() as u32); }
        let before = Instant::now();
        match capturer.frame(Duration::from_millis(16)) {
            Ok(frame) => {
                total += before.elapsed();
                count += 1;
                first_ms.get_or_insert(start.elapsed().as_millis());
                if motion {
                    if let Frame::PixelBuffer(pixels) = frame {
                        let stride = pixels.stride()[0];
                        let data = pixels.data();
                        // Only inspect the synthetic window: background and a
                        // moving green bar in BGRA, including top-down orientation.
                        let background = (110 * stride) + 110 * 4;
                        let row = 280 * stride;
                        let positions: Vec<_> = (100..740).filter(|x| {
                            data.get(row + x * 4..row + x * 4 + 3) == Some(&[0x40, 0xD0, 0x80])
                        }).collect();
                        if data.get(background..background + 3) == Some(&[0x20, 0x20, 0x20])
                            && positions.len() == 80 {
                            verified += 1;
                            if let Some(position) = positions.first().copied() {
                                if last_position.is_some() && last_position != Some(position) { changes += 1; }
                                last_position = Some(position);
                            }
                        }
                    }
                }
            }
            Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => { idle += 1; }
            Err(e) => return Err(e.into()),
        }
    }
    println!("CAPTURE_PROBE gdi={} frames={} idle={} first_ms={first_ms:?} capture_mean_ms={:.3}",
        capturer.is_gdi(), count, idle, total.as_secs_f64() * 1000.0 / count.max(1) as f64);
    println!("CAPTURE_PROBE verified_pattern={verified} movement_changes={changes} fps={:.1}",
        count as f64 / start.elapsed().as_secs_f64());
    if motion && (verified == 0 || changes == 0) { return Err("No valid moving test pattern captured".into()); }
    Ok(())
}

#[cfg(windows)]
struct MotionWindow(winapi::shared::windef::HWND);

#[cfg(windows)]
impl MotionWindow {
    fn new() -> std::io::Result<Self> {
        use winapi::um::winuser::*;
        let class: Vec<u16> = "STATIC\0".encode_utf16().collect();
        let title: Vec<u16> = "P2PDesk capture probe (5 seconds)\0".encode_utf16().collect();
        let hwnd = unsafe { CreateWindowExW(WS_EX_TOPMOST | WS_EX_NOACTIVATE,
            class.as_ptr(), title.as_ptr(), WS_POPUP | WS_VISIBLE,
            100, 100, 640, 360, std::ptr::null_mut(), std::ptr::null_mut(),
            std::ptr::null_mut(), std::ptr::null_mut()) };
        if hwnd.is_null() { return Err(std::io::Error::last_os_error()); }
        Ok(Self(hwnd))
    }

    fn draw(&self, tick: u32) {
        use winapi::{shared::windef::RECT, um::{winuser::*, wingdi::*}};
        unsafe {
            let mut msg = std::mem::zeroed();
            while PeekMessageW(&mut msg, self.0, 0, 0, PM_REMOVE) != 0 {
                TranslateMessage(&msg);
                DispatchMessageW(&msg);
            }
            let dc = GetDC(self.0);
            let background = CreateSolidBrush(0x202020);
            FillRect(dc, &RECT { left: 0, top: 0, right: 640, bottom: 360 }, background);
            DeleteObject(background as _);
            let brush = CreateSolidBrush(0x40D080);
            let x = (tick / 4 % 560) as i32;
            FillRect(dc, &RECT { left: x, top: 40, right: x + 80, bottom: 320 }, brush);
            DeleteObject(brush as _);
            GdiFlush();
            ReleaseDC(self.0, dc);
        }
    }
}

#[cfg(windows)]
impl Drop for MotionWindow {
    fn drop(&mut self) { unsafe { winapi::um::winuser::DestroyWindow(self.0); } }
}
#[cfg(not(windows))]
fn main() { eprintln!("This probe requires Windows."); }
