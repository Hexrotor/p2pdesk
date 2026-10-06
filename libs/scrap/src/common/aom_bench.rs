//! Explicitly invoked encoding research; never linked into production builds.
use super::*;
use std::{
    io::{Read, Write},
    net::{Shutdown, TcpListener, TcpStream},
    thread,
    time::{Duration, Instant},
};

fn pattern(width: usize, height: usize, index: usize, fps: usize) -> Vec<u8> {
    let mut data = vec![128; width * height * 3 / 2];
    let scroll = index * 240 / fps;
    for y in 0..height {
        let sy = y + scroll;
        for x in 0..width {
            let cell = (x / 8).wrapping_mul(73) ^ (sy / 16).wrapping_mul(157);
            let ink = x % 8 < 5
                && sy % 16 < 10
                && ((cell.rotate_left((sy % 7) as u32) >> (x % 5)) & 1) != 0;
            data[y * width + x] = if ink { 35 } else { 230 };
            // A moving textured panel makes the scene more demanding than static text.
            if x > width / 2 && y > height / 2 {
                let px = (x + scroll) as u32;
                let py = (y + scroll / 2) as u32;
                data[y * width + x] = ((px * 13 ^ py * 29 ^ (px * py / 97)) % 192 + 32) as u8;
            }
        }
    }
    data
}

fn percentile(values: &[f64], pct: usize) -> f64 {
    let mut v = values.to_vec();
    v.sort_by(f64::total_cmp);
    v[((v.len() - 1) * pct + 99) / 100]
}

// Explicit CBR initialization keeps historical research baselines reproducible.
fn cq_research_encoder(width: u32, height: u32) -> ResultType<AomEncoder> {
    let mut encoder = AomEncoder::new(
        crate::codec::EncoderCfg::AOM(AomEncoderConfig {
            width,
            height,
            quality: 1.0,
            keyframe_interval: None,
            cq: false,
        }),
        false,
    )?;
    let mut cfg = unsafe { *encoder.ctx.config.enc };
    cfg.g_threads = 8;
    call_aom!(aom_codec_enc_config_set(&mut encoder.ctx, &cfg));
    webrtc::set_controls(&mut encoder.ctx, &cfg)?;
    Ok(encoder)
}

fn configure_dynamic_cq(
    encoder: &mut AomEncoder,
    cq: Option<u32>,
    target_kbps: u32,
    fps: u32,
    speed: i32,
) -> ResultType<()> {
    encoder.set_frame_rate(fps);
    let mut cfg = unsafe { *encoder.ctx.config.enc };
    cfg.rc_end_usage = if cq.is_some() {
        aom_rc_mode::AOM_CQ
    } else {
        aom_rc_mode::AOM_CBR
    };
    cfg.rc_target_bitrate = target_kbps;
    call_aom!(aom_codec_enc_config_set(&mut encoder.ctx, &cfg));
    if let Some(level) = cq {
        call_aom!(aom_codec_control(
            &mut encoder.ctx,
            aome_enc_control_id::AOME_SET_CQ_LEVEL as i32,
            level
        ));
    }
    call_aom!(aom_codec_control(
        &mut encoder.ctx,
        aome_enc_control_id::AOME_SET_CPUUSED as i32,
        speed
    ));
    Ok(())
}

#[test]
fn av1_cq_live_controls_keep_one_continuous_stream() -> ResultType<()> {
    let (width, height) = (640, 360);
    let mut encoder = cq_research_encoder(width, height)?;
    let original_context = encoder.ctx.priv_;
    let target = unsafe { (*encoder.ctx.config.enc).rc_target_bitrate };
    let mut decoder = AomDecoder::new()?;
    let mut pts = 1000i64;
    let mut count = 0;
    // Change one axis at a time, including mode switches in the same encoder.
    for (stage, (cq, scale, fps)) in [
        (Some(15), 1.0, 30),
        (Some(15), 1.0, 120),
        (Some(15), 0.5, 120),
        (Some(20), 0.5, 120),
        (Some(15), 1.0, 60),
        (None, 1.0, 60),
        (Some(15), 1.0, 60),
    ]
    .iter()
    .copied()
    .enumerate()
    {
        configure_dynamic_cq(
            &mut encoder,
            cq,
            (f64::from(target) * scale) as u32,
            fps,
            10,
        )?;
        assert_eq!(
            encoder.ctx.priv_, original_context,
            "encoder was reconstructed"
        );
        let mut keys = 0;
        let mut bytes = 0;
        for i in 0..60 {
            let input = pattern(width as usize, height as usize, count, fps as usize);
            let packets: Vec<_> = encoder
                .encode(pts, &input, 1)?
                .map(|f| (f.pts, f.key, f.data.to_vec()))
                .collect();
            assert_eq!(packets.len(), 1);
            assert_eq!(packets[0].0, pts);
            keys += usize::from(packets[0].1);
            bytes += packets[0].2.len();
            assert_eq!(decoder.decode(&packets[0].2)?.count(), 1);
            count += 1;
            pts += i64::from((i + 1) * 1000 / fps - i * 1000 / fps);
            if stage == 4 && i == 29 {
                pts += 3000;
            } // idle/resume without filler frames
        }
        println!("AV1_CQ_LIVE stage={stage} cq={cq:?} scale={scale} fps={fps} frames=60 keyframes={keys} bytes={bytes} same_encoder=true decoded=true");
    }
    // The existing application's ABR/quality setter must preserve CQ mode.
    encoder.set_quality(0.5)?;
    let updated = unsafe { *encoder.ctx.config.enc };
    assert_eq!(updated.rc_end_usage, aom_rc_mode::AOM_CQ);
    assert_eq!(
        (updated.rc_min_quantizer, updated.rc_max_quantizer),
        AomEncoder::calc_q_values(0.5)
    );
    assert_eq!(encoder.ctx.priv_, original_context);
    for i in 0..60 {
        let input = pattern(width as usize, height as usize, count, 60);
        let packets: Vec<_> = encoder
            .encode(pts, &input, 1)?
            .map(|f| (f.pts, f.data.to_vec()))
            .collect();
        assert_eq!(packets.len(), 1);
        assert_eq!(packets[0].0, pts);
        assert_eq!(decoder.decode(&packets[0].1)?.count(), 1);
        pts += (i + 1) * 1000 / 60 - i * 1000 / 60;
        count += 1;
    }
    println!("AV1_CQ_LIVE native_set_quality=0.5 cq_preserved=true qmin={} qmax={} target_kbps={} frames=60 decoded=true", updated.rc_min_quantizer, updated.rc_max_quantizer, updated.rc_target_bitrate);
    assert_eq!(count, 480);
    Ok(())
}

#[test]
fn av1_product_cq_and_cbr_quality_updates_decode_continuously() -> ResultType<()> {
    let (width, height) = (640, 384); // I420 plane strides are all aligned to 64.
    for cq in [false, true] {
        let mut encoder = AomEncoder::new(
            crate::codec::EncoderCfg::AOM(AomEncoderConfig {
                width, height, quality: 1.0, keyframe_interval: None, cq,
            }),
            false,
        )?;
        let context = encoder.ctx.priv_;
        let mut decoder = AomDecoder::new()?;
        let mut pts = 1000;
        let mut keys = 0;
        let mut total = 0;
        for (ratio, fps) in [(1.0, 30), (0.5, 120), (0.2, 60), (0.67, 30), (1.5, 60), (1.0, 120)] {
            encoder.set_frame_rate(fps);
            let duration = encoder.frame_duration;
            encoder.set_quality(ratio)?;
            let cfg = unsafe { *encoder.ctx.config.enc };
            assert_eq!(cfg.rc_end_usage, if cq { aom_rc_mode::AOM_CQ } else { aom_rc_mode::AOM_CBR });
            assert_eq!((cfg.rc_min_quantizer, cfg.rc_max_quantizer), AomEncoder::calc_q_values(ratio));
            assert_eq!(encoder.frame_duration, duration, "quality must not change FPS");
            let mut bytes = 0;
            for i in 0..60 {
                let input = pattern(width as _, height as _, i, fps as _);
                let video = encoder.encode_to_message(EncodeInput::YUV(&input), pts)?;
                let frames = &video.av1s().frames;
                assert_eq!(frames.len(), 1);
                assert_eq!(frames[0].pts, pts);
                assert_eq!(decoder.decode(&frames[0].data)?.count(), 1);
                keys += usize::from(frames[0].key);
                bytes += frames[0].data.len();
                total += 1;
                pts += ((i + 1) * 1000 / fps as usize - i * 1000 / fps as usize) as i64;
            }
            assert_eq!(encoder.ctx.priv_, context);
            println!("AV1_PRODUCT cq={cq} ratio={ratio} fps={fps} target={} q={}..{} frames=60 bytes={bytes} decoded=true", cfg.rc_target_bitrate, cfg.rc_min_quantizer, cfg.rc_max_quantizer);
            pts += 3000; // Resume without synthesizing idle frames.
        }
        for ratio in [f32::NAN, f32::INFINITY, 0.0, -1.0] {
            let before = unsafe { *encoder.ctx.config.enc };
            assert!(encoder.set_quality(ratio).is_err());
            assert_eq!(unsafe { (*encoder.ctx.config.enc).rc_target_bitrate }, before.rc_target_bitrate);
        }
        assert_eq!(keys, 1, "quality and FPS changes must not force keyframes");
        assert_eq!(total, 360);
    }
    Ok(())
}

#[test]
#[ignore = "explicit CQ runtime control and quality comparison on fixed browser corpus"]
fn av1_cq_dynamic_browser_comparison() -> ResultType<()> {
    let data = std::fs::read(std::env::var("AV1_BENCH_INPUT")?)?;
    let (width, height, fps) = (1920usize, 1080usize, 60usize);
    let frame_size = width * height * 3 / 2;
    let count = 360;
    hbb_common::anyhow::ensure!(
        data.len() == count * frame_size,
        "expected fixed 360-frame 1080p I420 corpus"
    );
    let speed = std::env::var("AV1_DYNAMIC_SPEED")
        .unwrap_or_else(|_| "8".into())
        .parse::<i32>()?;
    hbb_common::anyhow::ensure!((8..=10).contains(&speed), "invalid speed");
    // Repeating the exact clip makes both the source sequence and the cycle
    // boundaries identical between policies. No encoder/decoder resets.
    for policy in ["fixed", "target", "target_cq"] {
        let mut encoder = cq_research_encoder(width as _, height as _)?;
        let target = unsafe { (*encoder.ctx.config.enc).rc_target_bitrate };
        let original_context = encoder.ctx.priv_;
        let mut decoder = AomDecoder::new()?;
        let mut last_controls = None;
        for stage in 0..5 {
            let reduced = policy != "fixed" && (stage == 1 || stage == 2);
            let scale = if reduced { 0.5 } else { 1.0 };
            let cq = if policy == "target_cq" && stage == 2 {
                20
            } else {
                15
            };
            let controls = (cq, (f64::from(target) * scale).round() as u32);
            if last_controls != Some(controls) {
                configure_dynamic_cq(&mut encoder, Some(cq), controls.1, fps as u32, speed)?;
                last_controls = Some(controls);
            }
            assert_eq!(encoder.ctx.priv_, original_context);
            let mut packets = Vec::with_capacity(count);
            let mut times = Vec::with_capacity(count);
            let mut quantizers = Vec::with_capacity(count);
            let mut keys = 0;
            for (i, input) in data.chunks_exact(frame_size).enumerate() {
                let pts =
                    ((stage * count + i) * 1000 / fps + if stage == 4 { 3000 } else { 0 }) as i64;
                let started = Instant::now();
                let frames: Vec<_> = encoder
                    .encode(pts, input, 1)?
                    .map(|f| (f.pts, f.key, f.data.to_vec()))
                    .collect();
                times.push(started.elapsed().as_secs_f64() * 1000.0);
                assert_eq!(frames.len(), 1);
                assert_eq!(frames[0].0, pts);
                keys += usize::from(frames[0].1);
                packets.push(frames.into_iter().next().unwrap().2);
                let mut q = 0i32;
                call_aom!(aom_codec_control(
                    &mut encoder.ctx,
                    aome_enc_control_id::AOME_GET_LAST_QUANTIZER_64 as i32,
                    &mut q as *mut i32
                ));
                quantizers.push(q);
            }
            let mut error = 0.0f64;
            let mut edge_error = 0.0f64;
            let mut edge_count = 0usize;
            for (packet, original) in packets.iter().zip(data.chunks_exact(frame_size)) {
                let mut decoded = 0;
                for image in decoder.decode(packet)? {
                    let image = image.inner();
                    assert_eq!((image.d_w as usize, image.d_h as usize), (width, height));
                    for y in 0..height {
                        let row = unsafe {
                            slice::from_raw_parts(
                                image.planes[0].add(y * image.stride[0] as usize),
                                width,
                            )
                        };
                        for (x, (&a, &b)) in row
                            .iter()
                            .zip(&original[y * width..(y + 1) * width])
                            .enumerate()
                        {
                            let e = (f64::from(a) - f64::from(b)).powi(2);
                            error += e;
                            if (x > 0 && b.abs_diff(original[y * width + x - 1]) > 16)
                                || (y > 0 && b.abs_diff(original[(y - 1) * width + x]) > 16)
                            {
                                edge_error += e;
                                edge_count += 1;
                            }
                        }
                    }
                    decoded += 1;
                }
                assert_eq!(decoded, 1);
            }
            let per_second_kbps: Vec<_> = packets
                .chunks(fps)
                .map(|c| c.iter().map(Vec::len).sum::<usize>() as f64 * 8.0 / 1000.0)
                .collect();
            let bytes = packets.iter().map(Vec::len).sum::<usize>();
            let y_psnr = 10.0 * (255.0 * 255.0 * (width * height * count) as f64 / error).log10();
            let edge_psnr = 10.0 * (255.0 * 255.0 * edge_count as f64 / edge_error).log10();
            println!("AV1_CQ_DYNAMIC policy={policy} stage={stage} fps={fps} speed={speed} cq={cq} target_kbps={} frames={count} bytes={bytes} kbps={:.1} y_psnr={y_psnr:.3} edge_y_psnr={edge_psnr:.3} p95_ms={:.3} first_ms={:.3} keyframes={keys} q_min={} q_max={} q_mean={:.2} per_second_kbps={per_second_kbps:?}",
                (f64::from(target) * scale).round(), bytes as f64 * 8.0 / 6.0 / 1000.0, percentile(&times[1..], 95), times[0],
                quantizers.iter().min().unwrap(), quantizers.iter().max().unwrap(), quantizers.iter().sum::<i32>() as f64 / count as f64);
        }
    }
    Ok(())
}

#[test]
#[ignore = "explicit local AV1 encoding benchmark; several minutes"]
fn av1_encoding_loopback_matrix() -> ResultType<()> {
    let width = std::env::var("AV1_BENCH_WIDTH")
        .unwrap_or_else(|_| "1920".into())
        .parse::<usize>()?;
    let height = std::env::var("AV1_BENCH_HEIGHT")
        .unwrap_or_else(|_| "1080".into())
        .parse::<usize>()?;
    hbb_common::anyhow::ensure!(
        width > 0
            && height > 0
            && width <= 3840
            && height <= 2160
            && width % 2 == 0
            && height % 2 == 0,
        "invalid I420 dimensions"
    );
    let count = std::env::var("AV1_BENCH_FRAMES")
        .unwrap_or_else(|_| "120".to_owned())
        .parse::<usize>()?;
    hbb_common::anyhow::ensure!(
        (30..=720).contains(&count),
        "AV1_BENCH_FRAMES must be 30..720"
    );
    let external = std::env::var("AV1_BENCH_INPUT")
        .ok()
        .map(std::fs::read)
        .transpose()?;
    let frame_size = width * height * 3 / 2;
    if let Some(data) = &external {
        hbb_common::anyhow::ensure!(
            data.len() == frame_size * count,
            "input size does not match dimensions and frame count"
        );
    }
    let rates = if external.is_some() || std::env::var_os("AV1_BENCH_FPS").is_some() {
        let fps = std::env::var("AV1_BENCH_FPS")
            .unwrap_or_else(|_| "60".into())
            .parse::<usize>()?;
        hbb_common::anyhow::ensure!((1..=120).contains(&fps), "invalid input fps");
        vec![fps]
    } else {
        vec![30, 60, 120]
    };
    let version = unsafe { std::ffi::CStr::from_ptr(aom_codec_version_str()) }.to_string_lossy();
    let rate_mode = std::env::var("AV1_BENCH_RATE_CONTROL").unwrap_or_else(|_| "cbr".into());
    hbb_common::anyhow::ensure!(
        matches!(rate_mode.as_str(), "cbr" | "cq"),
        "invalid rate mode"
    );
    let cq_level = std::env::var("AV1_BENCH_CQ_LEVEL")
        .unwrap_or_else(|_| "28".into())
        .parse::<u32>()?;
    hbb_common::anyhow::ensure!(cq_level <= 63, "invalid CQ level");
    let scene = if external.is_some() {
        "external_i420"
    } else {
        "synthetic_scroll_and_texture"
    };
    println!("AV1_BENCH library={version} scene={scene} size={width}x{height} frames={count} chroma=I420 threads=per_case paced=false");
    for fps in rates {
        // Precompute outside timed encoding; identical frames for every configuration at this fps.
        let inputs: Vec<_> = match &external {
            Some(data) => data.chunks_exact(frame_size).map(<[u8]>::to_vec).collect(),
            None => (0..count).map(|i| pattern(width, height, i, fps)).collect(),
        };
        // speed, auto tiles, legacy 1 ms duration, IntraBC, threads, bitrate multiplier.
        // Quantizer bounds stay fixed when changing the target bitrate.
        let cases = match std::env::var("AV1_BENCH_CASES")
            .as_deref()
            .unwrap_or("default")
        {
            "default" => vec![
                (10, false, false, false, 8, 1.0),
                (9, false, false, false, 8, 1.0),
                (8, false, false, false, 8, 1.0),
                (10, true, false, false, 8, 1.0),
                (10, false, true, false, 8, 1.0),
            ],
            "tools" => vec![
                (10, false, false, false, 8, 1.0),
                (10, false, false, true, 8, 1.0),
                (10, true, false, true, 8, 1.0),
                (10, false, false, false, 4, 1.0),
                (10, true, false, false, 4, 1.0),
                (10, false, false, false, 2, 1.0),
                (10, true, false, false, 2, 1.0),
            ],
            "threads" => [1, 2, 4, 8]
                .iter()
                .map(|threads| (10, false, false, false, *threads, 1.0))
                .collect(),
            "cq" => [10, 8]
                .iter()
                .copied()
                .flat_map(|speed| {
                    [0.5, 0.75, 1.0]
                        .iter()
                        .copied()
                        .map(move |scale| (speed, false, false, false, 8, scale))
                })
                .collect(),
            "realtime" => vec![
                (10, false, false, false, 8, 1.0),
                (8, false, false, false, 8, 1.0),
            ],
            "rate" => [10, 9, 8]
                .iter()
                .copied()
                .flat_map(|speed| {
                    [0.5, 0.75, 1.0]
                        .iter()
                        .copied()
                        .map(move |scale| (speed, false, false, false, 8, scale))
                })
                .collect(),
            other => return Err(anyhow!("unknown AV1_BENCH_CASES: {other}")),
        };
        let mut baseline_packets = None;
        for (speed, auto_tiles, legacy_duration, intrabc, threads, bitrate_scale) in cases {
            let enc_cfg = AomEncoderConfig {
                width: width as _,
                height: height as _,
                quality: 1.0,
                keyframe_interval: None,
                cq: false,
            };
            let mut encoder = AomEncoder::new(crate::codec::EncoderCfg::AOM(enc_cfg), false)?;
            encoder.set_frame_rate(fps as u32);
            let mut config = unsafe { *encoder.ctx.config.enc };
            if rate_mode == "cq" {
                config.rc_end_usage = aom_rc_mode::AOM_CQ;
            }
            config.g_threads = threads;
            config.rc_target_bitrate =
                (f64::from(config.rc_target_bitrate) * bitrate_scale).round() as u32;
            call_aom!(aom_codec_enc_config_set(&mut encoder.ctx, &config));
            webrtc::set_controls(&mut encoder.ctx, &config)?;
            if rate_mode == "cq" {
                call_aom!(aom_codec_control(
                    &mut encoder.ctx,
                    aome_enc_control_id::AOME_SET_CQ_LEVEL as i32,
                    cq_level
                ));
            }
            call_aom!(aom_codec_control(
                &mut encoder.ctx,
                aome_enc_control_id::AOME_SET_CPUUSED as i32,
                speed
            ));
            if intrabc {
                call_aom!(aom_codec_control(
                    &mut encoder.ctx,
                    aome_enc_control_id::AV1E_SET_ENABLE_INTRABC as i32,
                    1i32
                ));
            }
            if auto_tiles {
                call_aom!(aom_codec_control(
                    &mut encoder.ctx,
                    aome_enc_control_id::AV1E_SET_AUTO_TILES as i32,
                    1i32
                ));
            }

            // Actual localhost TCP transfer; no desktop/session authentication is involved.
            let listener = TcpListener::bind("127.0.0.1:0")?;
            let address = listener.local_addr()?;
            let reader = thread::spawn(move || -> std::io::Result<Vec<Vec<u8>>> {
                let (mut stream, _) = listener.accept()?;
                stream.set_read_timeout(Some(Duration::from_secs(30)))?;
                let mut frames = Vec::new();
                loop {
                    let mut header = [0u8; 4];
                    stream.read_exact(&mut header)?;
                    let size = u32::from_le_bytes(header) as usize;
                    if size == 0 {
                        break;
                    }
                    if size > 32 * 1024 * 1024 {
                        return Err(std::io::Error::new(
                            std::io::ErrorKind::InvalidData,
                            "oversized frame",
                        ));
                    }
                    let mut data = vec![0; size];
                    stream.read_exact(&mut data)?;
                    frames.push(data);
                }
                Ok(frames)
            });
            let mut stream = TcpStream::connect(address)?;
            stream.set_nodelay(true)?;
            stream.set_write_timeout(Some(Duration::from_secs(30)))?;
            let mut durations = Vec::new();
            let mut sizes = Vec::new();
            for (i, input) in inputs.iter().enumerate() {
                let pts = (i * 1000 / fps) as i64;
                let started = Instant::now();
                let packets = if legacy_duration {
                    let mut image = aom_image_t::default();
                    call_aom_ptr!(aom_img_wrap(
                        &mut image,
                        aom_img_fmt::AOM_IMG_FMT_I420,
                        width as _,
                        height as _,
                        1,
                        input.as_ptr() as _
                    ));
                    let duration = (webrtc::kTimeBaseDen / 1000) as std::os::raw::c_ulong;
                    let encoder_pts = pts * (webrtc::kTimeBaseDen / 1000);
                    call_aom!(aom_codec_encode(
                        &mut encoder.ctx,
                        &image,
                        encoder_pts,
                        duration,
                        0
                    ));
                    EncodeFrames {
                        ctx: &mut encoder.ctx,
                        iter: ptr::null(),
                    }
                    .map(|frame| frame.data.to_vec())
                    .collect::<Vec<_>>()
                } else {
                    encoder
                        .encode(pts, input, 1)?
                        .map(|frame| frame.data.to_vec())
                        .collect::<Vec<_>>()
                };
                durations.push(started.elapsed().as_secs_f64() * 1000.0);
                assert_eq!(packets.len(), 1, "zero-lag encoder must produce one frame");
                for packet in packets {
                    sizes.push(packet.len());
                    stream.write_all(&(packet.len() as u32).to_le_bytes())?;
                    stream.write_all(&packet)?;
                }
            }
            stream.write_all(&0u32.to_le_bytes())?;
            stream.shutdown(Shutdown::Write)?;
            let packets = reader
                .join()
                .map_err(|_| anyhow!("loopback reader panicked"))??;
            assert_eq!(packets.len(), count);
            assert_eq!(
                packets.iter().map(Vec::len).sum::<usize>(),
                sizes.iter().sum::<usize>()
            );
            if speed == 10
                && !auto_tiles
                && !legacy_duration
                && !intrabc
                && threads == 8
                && bitrate_scale == 1.0
            {
                baseline_packets = Some(packets.clone());
            }
            if legacy_duration {
                println!(
                    "AV1_LEGACY_DURATION_COMPARE fps={fps} bitstream_identical={}",
                    baseline_packets.as_ref() == Some(&packets)
                );
            }

            // Decode only after timing all encodes, so decoder work cannot pollute encode timings.
            let mut decoder = AomDecoder::new()?;
            let mut squared_error = 0f64;
            let mut chroma_error = [0f64; 2];
            let mut edge_error = 0f64;
            let mut edge_pixels = 0usize;
            let mut pixels = 0usize;
            for (index, (packet, original)) in packets.iter().zip(&inputs).enumerate() {
                let mut decoded = 0;
                for image in decoder.decode(packet)? {
                    let image = image.inner();
                    assert_eq!((image.d_w as usize, image.d_h as usize), (width, height));
                    if index == count / 2 || index == count - 1 {
                        if let Ok(directory) = std::env::var("AV1_BENCH_DUMP_DIR") {
                            std::fs::create_dir_all(&directory)?;
                            let mut output = Vec::with_capacity(frame_size);
                            for plane in 0..3 {
                                let divisor = if plane == 0 { 1 } else { 2 };
                                for y in 0..height / divisor {
                                    output.extend_from_slice(unsafe {
                                        slice::from_raw_parts(
                                            image.planes[plane]
                                                .add(y * image.stride[plane] as usize),
                                            width / divisor,
                                        )
                                    });
                                }
                            }
                            let mode_suffix = if rate_mode == "cq" {
                                format!("-cq{cq_level}")
                            } else {
                                String::new()
                            };
                            let name = format!("s{speed}-t{threads}-a{auto_tiles}-i{intrabc}-legacy{legacy_duration}-r{bitrate_scale}{mode_suffix}-f{index}.i420");
                            std::fs::write(std::path::Path::new(&directory).join(name), output)?;
                        }
                    }
                    for y in 0..height {
                        let row = unsafe {
                            slice::from_raw_parts(
                                image.planes[0].add(y * image.stride[0] as usize),
                                width,
                            )
                        };
                        for (x, (a, b)) in row
                            .iter()
                            .zip(&original[y * width..(y + 1) * width])
                            .enumerate()
                        {
                            let diff = f64::from(*a) - f64::from(*b);
                            squared_error += diff * diff;
                            if (x > 0 && b.abs_diff(original[y * width + x - 1]) > 16)
                                || (y > 0 && b.abs_diff(original[(y - 1) * width + x]) > 16)
                            {
                                edge_error += diff * diff;
                                edge_pixels += 1;
                            }
                        }
                    }
                    for plane in 1..=2 {
                        let offset = width * height + (plane - 1) * width * height / 4;
                        for y in 0..height / 2 {
                            let row = unsafe {
                                slice::from_raw_parts(
                                    image.planes[plane].add(y * image.stride[plane] as usize),
                                    width / 2,
                                )
                            };
                            for (a, b) in row.iter().zip(
                                &original[offset + y * width / 2..offset + (y + 1) * width / 2],
                            ) {
                                let diff = f64::from(*a) - f64::from(*b);
                                chroma_error[plane - 1] += diff * diff;
                            }
                        }
                    }
                    pixels += width * height;
                    decoded += 1;
                }
                assert_eq!(decoded, 1);
            }
            let non_key = &durations[1..];
            let sum_ms: f64 = durations.iter().sum();
            let psnr = 10.0 * ((255.0 * 255.0 * pixels as f64) / squared_error).log10();
            let u_psnr = 10.0 * ((255.0 * 255.0 * (pixels / 4) as f64) / chroma_error[0]).log10();
            let v_psnr = 10.0 * ((255.0 * 255.0 * (pixels / 4) as f64) / chroma_error[1]).log10();
            let edge_psnr = if edge_pixels == 0 {
                f64::NAN
            } else {
                10.0 * ((255.0 * 255.0 * edge_pixels as f64) / edge_error).log10()
            };
            let per_second_kbps: Vec<f64> = sizes
                .chunks(fps)
                .map(|chunk| {
                    chunk.iter().sum::<usize>() as f64 * 8.0 * fps as f64
                        / chunk.len() as f64
                        / 1000.0
                })
                .collect();
            let peak_kbps = |window: usize| {
                sizes
                    .windows(window.min(sizes.len()))
                    .map(|chunk| {
                        chunk.iter().sum::<usize>() as f64 * 8.0 * fps as f64
                            / chunk.len() as f64
                            / 1000.0
                    })
                    .fold(0.0, f64::max)
            };
            println!("AV1_TRAFFIC fps={fps} speed={speed} bitrate_scale={bitrate_scale} rate_mode={rate_mode} cq_level={cq_level} peak_1s_kbps={:.1} peak_100ms_kbps={:.1} per_second_kbps={per_second_kbps:?}", peak_kbps(fps), peak_kbps((fps / 10).max(1)));
            println!("AV1_RESULT fps={fps} speed={speed} auto_tiles={auto_tiles} duration_by_fps={} intrabc={intrabc} threads={threads} bitrate_scale={bitrate_scale} rate_mode={rate_mode} cq_level={cq_level} target_kbps={} qmin={} qmax={} u_psnr={u_psnr:.3} v_psnr={v_psnr:.3} edge_y_psnr={edge_psnr:.3} frames={} kbps={:.1} y_psnr={:.3} key_ms={:.3} p50_ms={:.3} p95_ms={:.3} p99_ms={:.3} encode_capacity_fps={:.1} over_budget={} key_bytes={} total_bytes={} tail_kbps={:.1}",
                !legacy_duration, config.rc_target_bitrate, config.rc_min_quantizer, config.rc_max_quantizer,
                packets.len(), sizes.iter().sum::<usize>() as f64 * 8.0 * fps as f64 / count as f64 / 1000.0,
                psnr, durations[0], percentile(non_key, 50), percentile(non_key, 95), percentile(non_key, 99),
                count as f64 * 1000.0 / sum_ms, non_key.iter().filter(|t| **t > 1000.0 / fps as f64).count(), sizes[0], sizes.iter().sum::<usize>(),
                sizes[count / 2..].iter().sum::<usize>() as f64 * 8.0 * fps as f64 / (count - count / 2) as f64 / 1000.0);
        }
    }
    Ok(())
}

#[test]
fn av1_frame_budget_and_wire_timestamps_survive_rate_changes() -> ResultType<()> {
    let mut encoder = AomEncoder::new(
        crate::codec::EncoderCfg::AOM(AomEncoderConfig {
            width: 320,
            height: 180,
            quality: 1.0,
            keyframe_interval: None,
            cq: false,
        }),
        false,
    )?;
    let mut decoder = AomDecoder::new()?;
    let mut ms = 1234;
    for fps in [30, 60, 120, 1, 60] {
        encoder.set_frame_rate(fps);
        let config = unsafe { *encoder.ctx.config.enc };
        let budget_seconds = f64::from(config.g_timebase.num) * encoder.frame_duration as f64
            / f64::from(config.g_timebase.den);
        assert!((budget_seconds - 1.0 / f64::from(fps)).abs() < 1e-9);
        for i in 0..4 {
            let input = pattern(320, 180, i, fps as usize);
            let frames = encoder
                .encode(ms, &input, 1)?
                .map(|frame| (frame.pts, frame.data.to_vec()))
                .collect::<Vec<_>>();
            assert_eq!(frames.len(), 1);
            assert_eq!(frames[0].0, ms, "wire PTS must remain in milliseconds");
            assert_eq!(decoder.decode(&frames[0].1)?.count(), 1);
            ms += i64::from(1000 / fps);
        }
        ms += 2000; // No duplicate frames while idle; resume with the original clock.
    }
    println!("AV1_TIMING frames=20 fps=30,60,120,1,60 wire_pts_ms=true idle_resume=true");
    Ok(())
}

#[test]
#[ignore = "paced local encoding, idle/resume and TCP stream verification"]
fn adaptive_speed_stream_roundtrip() -> ResultType<()> {
    let (width, height) = (640, 360);
    let mut encoder = AomEncoder::new(
        crate::codec::EncoderCfg::AOM(AomEncoderConfig {
            width,
            height,
            quality: 1.0,
            keyframe_interval: None,
            cq: false,
        }),
        false,
    )?;
    let original = unsafe { *encoder.ctx.config.enc };
    let mut speeds = vec![encoder.speed_control.speed];
    let listener = TcpListener::bind("127.0.0.1:0")?;
    let address = listener.local_addr()?;
    let receiver = thread::spawn(move || -> std::io::Result<Vec<Vec<u8>>> {
        let (mut stream, _) = listener.accept()?;
        stream.set_read_timeout(Some(Duration::from_secs(20)))?;
        let mut packets = Vec::new();
        loop {
            let mut header = [0; 4];
            stream.read_exact(&mut header)?;
            let len = u32::from_le_bytes(header) as usize;
            if len == 0 {
                return Ok(packets);
            }
            if len > 32 * 1024 * 1024 {
                return Err(std::io::Error::new(
                    std::io::ErrorKind::InvalidData,
                    "oversized frame",
                ));
            }
            let mut packet = vec![0; len];
            stream.read_exact(&mut packet)?;
            packets.push(packet);
        }
    });
    let mut stream = TcpStream::connect(address)?;
    stream.set_nodelay(true)?;
    stream.set_write_timeout(Some(Duration::from_secs(20)))?;
    let started = Instant::now();
    let mut frames_sent = 0;
    for (fps, count) in [(30, 100), (120, 120), (60, 120)] {
        encoder.set_frame_rate(fps);
        let phase_start = Instant::now();
        for i in 0..count {
            let target = phase_start + Duration::from_secs_f64(i as f64 / fps as f64);
            if let Some(wait) = target.checked_duration_since(Instant::now()) {
                thread::sleep(wait);
            }
            let pixels = pattern(width as _, height as _, i, fps as _);
            let vf = encoder.encode_to_message(
                EncodeInput::YUV(&pixels),
                started.elapsed().as_millis() as _,
            )?;
            let Some(hbb_common::message_proto::video_frame::Union::Av1s(frames)) = vf.union else {
                return Err(anyhow!("expected AV1 video frame"));
            };
            for frame in frames.frames {
                stream.write_all(&(frame.data.len() as u32).to_le_bytes())?;
                stream.write_all(&frame.data)?;
                frames_sent += 1;
            }
            let speed = encoder.speed_control.speed;
            if speeds.last() != Some(&speed) {
                speeds.push(speed);
            }
        }
        // The capture loop produces no frames during idle; do not feed duplicate frames.
        thread::sleep(Duration::from_millis(600));
    }
    stream.write_all(&0u32.to_le_bytes())?;
    stream.shutdown(Shutdown::Write)?;
    let packets = receiver
        .join()
        .map_err(|_| anyhow!("receiver panicked"))??;
    assert_eq!(packets.len(), frames_sent);
    assert_eq!(frames_sent, 340);
    assert!(
        speeds.contains(&8),
        "must actually exercise a runtime speed switch: {:?}",
        speeds
    );
    let updated = unsafe { *encoder.ctx.config.enc };
    assert_eq!(updated.rc_target_bitrate, original.rc_target_bitrate);
    assert_eq!(updated.rc_min_quantizer, original.rc_min_quantizer);
    assert_eq!(updated.rc_max_quantizer, original.rc_max_quantizer);
    assert_eq!(updated.g_lag_in_frames, 0);
    let mut decoder = AomDecoder::new()?;
    let mut decoded = 0;
    for packet in packets {
        for image in decoder.decode(&packet)? {
            assert_eq!((image.inner().d_w, image.inner().d_h), (width, height));
            decoded += 1;
        }
    }
    assert_eq!(decoded, frames_sent);
    println!("AV1_ADAPTIVE frames={decoded} fps_phases=30,120,60 speeds={speeds:?} bitrate_unchanged=true quantizers_unchanged=true idle_resume=true");
    Ok(())
}
