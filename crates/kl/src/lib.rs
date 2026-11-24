pub mod db;
pub mod index;
pub mod install;
pub mod manifest;
pub mod repo;

pub const NAMESPACE: &str = "kl@kido.sh";
pub const ARCHES: &[&str] = &["arm64", "arm64e"];

pub fn check_channel(channel: &str) -> anyhow::Result<()> {
    anyhow::ensure!(
        !channel.is_empty()
            && channel.len() <= 32
            && channel
                .bytes()
                .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || b == b'-'),
        "bad channel {channel:?}"
    );
    Ok(())
}

pub fn check_arch(arch: &str) -> anyhow::Result<()> {
    anyhow::ensure!(
        ARCHES.contains(&arch),
        "unknown architecture {arch:?}, expected arm64 or arm64e"
    );
    Ok(())
}
