// Probe real hardware codec availability without changing application settings.
#[cfg(all(windows, feature = "hwcodec"))]
fn main() -> hbb_common::ResultType<()> {
    let result = scrap::hwcodec::check_available_hwcodec();
    let config: hbb_common::serde_json::Value = hbb_common::serde_json::from_str(&result)?;
    println!("CODEC_PROBE {}", hbb_common::serde_json::to_string(&config)?);
    Ok(())
}

#[cfg(not(all(windows, feature = "hwcodec")))]
fn main() {
    eprintln!("Build on Windows with --features hwcodec,vram");
    std::process::exit(1);
}
