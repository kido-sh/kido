pub mod config;
pub mod db;
pub mod index;
pub mod install;
pub mod manifest;
pub mod repo;

pub use kido_trust::{ARCHES, CHANNELS, DEFAULT_CHANNEL, DEFAULT_KL_VERSION};

pub const NAMESPACE: &str = "kl@kido.sh";
pub const DEFAULT_REPO: &str = "https://kl.kido.sh";

pub fn check_kl_version(kl_version: &str) -> anyhow::Result<()> {
    kido_trust::check_kl_version(kl_version)?;
    Ok(())
}

pub fn check_build(build: &str) -> anyhow::Result<()> {
    kido_trust::check_build(build)?;
    Ok(())
}

pub fn check_channel(channel: &str) -> anyhow::Result<()> {
    kido_trust::check_channel(channel)?;
    Ok(())
}

pub fn check_arch(arch: &str) -> anyhow::Result<()> {
    kido_trust::check_arch(arch)?;
    Ok(())
}
