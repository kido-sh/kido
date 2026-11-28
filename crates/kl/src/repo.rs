use anyhow::{Context, Result, bail, ensure};
use std::collections::BTreeSet;
use std::fs::File;
use std::io::Read;
use std::net::{IpAddr, Ipv4Addr, Ipv6Addr};
use std::path::PathBuf;
use std::sync::{Arc, OnceLock};
use std::time::Duration;
use ureq::config::Config;
use ureq::http::Uri;
use ureq::unversioned::resolver::{DefaultResolver, ResolvedSocketAddrs, Resolver};
use ureq::unversioned::transport::{DefaultConnector, NextTimeout};

const RESOLVE_TIMEOUT: Duration = Duration::from_secs(15);
const CONNECT_TIMEOUT: Duration = Duration::from_secs(15);
const SEND_TIMEOUT: Duration = Duration::from_secs(15);
const RESPONSE_TIMEOUT: Duration = Duration::from_secs(30);
const PER_IO_TIMEOUT: Duration = Duration::from_secs(30);
const METADATA_GLOBAL_TIMEOUT: Duration = Duration::from_secs(60);

static HTTPS_TLS_CONFIG: OnceLock<ureq::tls::TlsConfig> = OnceLock::new();

#[derive(Debug, Clone, Copy)]
struct PolicyResolver {
    allow_private_lan: bool,
}

impl Resolver for PolicyResolver {
    fn resolve(
        &self,
        uri: &Uri,
        config: &Config,
        timeout: NextTimeout,
    ) -> std::result::Result<ResolvedSocketAddrs, ureq::Error> {
        let resolved = DefaultResolver::default().resolve(uri, config, timeout)?;
        let mut allowed = self.empty();
        for &addr in &resolved {
            let ip = addr.ip().to_canonical();
            let ok = is_loopback_ip(ip) || (self.allow_private_lan && is_private_lan_ip(ip));
            if ok && allowed.len() < 16 {
                allowed.push(addr);
            }
        }
        if allowed.is_empty() {
            return Err(ureq::Error::Io(std::io::Error::new(
                std::io::ErrorKind::PermissionDenied,
                "resolved host IP violates cleartext HTTP loopback/LAN policy",
            )));
        }
        Ok(allowed)
    }
}

pub struct Repo {
    base: String,
    is_mirror: bool,
    agent: Option<ureq::Agent>,
}

impl Repo {
    pub fn new(base: &str) -> Result<Self> {
        Self::open_url(base, false)
    }

    pub fn new_mirror(base: &str) -> Result<Self> {
        Self::open_url(base, true)
    }

    pub fn validate_url(base: &str, is_mirror: bool) -> Result<()> {
        let base = base.trim_end_matches('/');
        validate_base_url(base)?;
        if let Some(root) = base.strip_prefix("file://") {
            validate_file_root(root)
        } else if base.starts_with("https://") {
            let host = extract_http_host(base, "https://")?;
            ensure!(!host.is_empty(), "empty https:// host in {base}");
            Ok(())
        } else if base.starts_with("http://") {
            let host = extract_http_host(base, "http://")?;
            if is_loopback_host(host) || (is_mirror && is_private_lan_host(host)) {
                Ok(())
            } else if is_mirror {
                bail!(
                    "cleartext http:// mirror {base} is only allowed for loopback or private LAN hosts; use https://"
                )
            } else {
                bail!(
                    "the repository must be an https://, file://, or loopback http:// URL, not {base}"
                )
            }
        } else {
            bail!("the repository must be an https:// or file:// URL, not {base}")
        }
    }

    fn open_url(base: &str, is_mirror: bool) -> Result<Self> {
        Self::validate_url(base, is_mirror)?;
        let base = base.trim_end_matches('/').to_owned();
        if base.starts_with("file://") {
            Ok(Repo {
                base,
                is_mirror,
                agent: None,
            })
        } else if base.starts_with("https://") {
            Ok(Repo {
                base,
                is_mirror,
                agent: Some(https_agent()?),
            })
        } else {
            Ok(Repo {
                base,
                is_mirror,
                agent: Some(http_agent(is_mirror)),
            })
        }
    }

    #[must_use]
    pub fn url(&self) -> &str {
        &self.base
    }

    #[must_use]
    pub fn is_mirror(&self) -> bool {
        self.is_mirror
    }

    pub fn open(&self, rel: &str) -> Result<Box<dyn Read>> {
        self.open_with_timeout(rel, None)
    }

    fn open_with_timeout(
        &self,
        rel: &str,
        global_timeout: Option<Duration>,
    ) -> Result<Box<dyn Read>> {
        check_repo_rel(rel)?;
        match &self.agent {
            None => {
                let root = self
                    .base
                    .strip_prefix("file://")
                    .ok_or_else(|| anyhow::anyhow!("bad file:// URL"))?;
                let path = PathBuf::from(root).join(rel);
                Ok(Box::new(
                    File::open(&path).with_context(|| format!("open {}", path.display()))?,
                ))
            }
            Some(agent) => {
                let url = format!("{}/{rel}", self.base);
                let mut req = agent.get(&url);
                if let Some(limit) = global_timeout {
                    req = req.config().timeout_global(Some(limit)).build();
                }
                let response = req.call().with_context(|| format!("GET {url}"))?;
                ensure!(
                    response.status() == 200,
                    "GET {url} returned HTTP {} (redirects and non-200 responses are refused)",
                    response.status()
                );
                Ok(Box::new(response.into_body().into_reader()))
            }
        }
    }

    pub fn read(&self, rel: &str, limit: u64) -> Result<Vec<u8>> {
        let mut buf = Vec::new();
        self.open_with_timeout(rel, Some(METADATA_GLOBAL_TIMEOUT))?
            .take(limit.saturating_add(1))
            .read_to_end(&mut buf)?;
        ensure!(
            buf.len() as u64 <= limit,
            "{rel} is larger than {limit} bytes"
        );
        Ok(buf)
    }
}

pub struct RepoSet {
    mirrors: Vec<Repo>,
    primary: Repo,
}

impl RepoSet {
    pub fn new(mirrors: &[String], primary: &str) -> Result<Self> {
        // Always validate primary under the stricter primary URL policy first.
        let primary_repo = Repo::new(primary)?;
        let norm_primary = primary_repo.url().to_owned();
        let mut mirror_repos = Vec::new();
        let mut seen = BTreeSet::new();
        seen.insert(norm_primary);
        for url in mirrors {
            let norm = url.trim_end_matches('/').to_owned();
            if seen.insert(norm) {
                mirror_repos.push(Repo::new_mirror(url)?);
            }
        }
        Ok(RepoSet {
            mirrors: mirror_repos,
            primary: primary_repo,
        })
    }

    #[must_use]
    pub fn primary(&self) -> &Repo {
        &self.primary
    }

    #[must_use]
    pub fn mirrors(&self) -> &[Repo] {
        &self.mirrors
    }

    /// Returns all sources ordered mirrors-first, falling back to the primary repository last.
    #[must_use]
    pub fn mirrors_then_primary(&self) -> Vec<&Repo> {
        let mut out = Vec::with_capacity(self.mirrors.len() + 1);
        out.extend(self.mirrors.iter());
        out.push(&self.primary);
        out
    }
}

fn validate_base_url(base: &str) -> Result<()> {
    ensure!(
        !base.is_empty()
            && !base.contains(['?', '#', '\\', '\0'])
            && !base.bytes().any(|b| b <= 0x20 || b == 0x7f),
        "bad repository URL {base:?}"
    );
    Ok(())
}

fn validate_file_root(root: &str) -> Result<()> {
    ensure!(
        root.starts_with('/')
            && !root.contains('\0')
            && root.split('/').skip(1).all(|c| c != "." && c != ".."),
        "bad file:// repository path {root:?}"
    );
    Ok(())
}

fn extract_http_host<'a>(url: &'a str, scheme: &str) -> Result<&'a str> {
    let rest = &url[scheme.len()..];
    let authority = rest.split('/').next().unwrap_or("");
    ensure!(
        !authority.is_empty() && !authority.contains('@'),
        "bad authority in repository URL {url:?}"
    );
    if let Some(bracketed) = authority.strip_prefix('[') {
        let Some(end) = bracketed.find(']') else {
            bail!("unclosed IPv6 host in {url:?}");
        };
        let host = &bracketed[..end];
        let after = &bracketed[end + 1..];
        if !after.is_empty() {
            let port = after
                .strip_prefix(':')
                .ok_or_else(|| anyhow::anyhow!("bad port in {url:?}"))?;
            ensure!(
                !port.is_empty() && port.parse::<u16>().is_ok(),
                "bad port in {url:?}"
            );
        }
        Ok(host)
    } else if let Some((host, port)) = authority.split_once(':') {
        ensure!(
            !host.is_empty() && !port.is_empty() && port.parse::<u16>().is_ok(),
            "bad host or port in {url:?}"
        );
        Ok(host)
    } else {
        Ok(authority)
    }
}

fn is_loopback_ip(ip: IpAddr) -> bool {
    match ip.to_canonical() {
        IpAddr::V4(v4) => v4.is_loopback(),
        IpAddr::V6(v6) => v6.is_loopback(),
    }
}

fn is_private_lan_ip(ip: IpAddr) -> bool {
    match ip.to_canonical() {
        // RFC 1918 private IPv4 only (deliberately excludes 169.254.0.0/16 cloud metadata IMDS).
        IpAddr::V4(v4) => v4.is_private(),
        IpAddr::V6(v6) => {
            let seg0 = v6.segments()[0];
            // Unique local (fc00::/7) or link-local (fe80::/10)
            (seg0 & 0xfe00) == 0xfc00 || (seg0 & 0xffc0) == 0xfe80
        }
    }
}

fn is_loopback_host(host: &str) -> bool {
    if host.eq_ignore_ascii_case("localhost") {
        return true;
    }
    if let Ok(ip) = host.parse::<Ipv4Addr>() {
        return ip.is_loopback();
    }
    if let Ok(ip) = host.parse::<Ipv6Addr>() {
        return is_loopback_ip(IpAddr::V6(ip));
    }
    false
}

fn is_private_lan_host(host: &str) -> bool {
    if let Ok(ip) = host.parse::<Ipv4Addr>() {
        return is_private_lan_ip(IpAddr::V4(ip));
    }
    if let Ok(ip) = host.parse::<Ipv6Addr>() {
        return is_private_lan_ip(IpAddr::V6(ip));
    }
    let lower = host.to_ascii_lowercase();
    if let Some((prefix, tld)) = lower.rsplit_once('.') {
        if !prefix.is_empty() && matches!(tld, "local" | "internal") {
            return true;
        }
    }
    // Single-label internal hostname (e.g. "kl-cache"). Reject numeric/hex/octal IPv4 encodings
    // like "134744072" or "0x08080808" that getaddrinfo would resolve to public IPv4 addresses.
    !lower.contains('.')
        && !lower.is_empty()
        && !lower.starts_with('-')
        && !lower.ends_with('-')
        && !lower.starts_with("0x")
        && lower.bytes().any(|b| b.is_ascii_alphabetic())
        && lower
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b == b'-')
}

pub fn check_repo_rel(rel: &str) -> Result<()> {
    ensure!(
        !rel.is_empty()
            && !rel.starts_with('/')
            && !rel.ends_with('/')
            && !rel.contains(['\\', '?', '#', '\0'])
            && !rel.bytes().any(|b| b <= 0x20 || b == 0x7f)
            && rel
                .split('/')
                .all(|c| !c.is_empty() && c != "." && c != ".."),
        "bad repository relative path {rel:?}"
    );
    Ok(())
}

fn http_agent(allow_private_lan: bool) -> ureq::Agent {
    let config = ureq::Agent::config_builder()
        .https_only(false)
        .proxy(None)
        .max_redirects(0)
        .timeout_resolve(Some(RESOLVE_TIMEOUT))
        .timeout_connect(Some(CONNECT_TIMEOUT))
        .timeout_send_request(Some(SEND_TIMEOUT))
        .timeout_recv_response(Some(RESPONSE_TIMEOUT))
        .timeout_per_read(Some(PER_IO_TIMEOUT))
        .timeout_per_write(Some(PER_IO_TIMEOUT))
        .build();
    ureq::Agent::with_parts(
        config,
        DefaultConnector::default(),
        PolicyResolver { allow_private_lan },
    )
}

fn load_https_tls() -> Result<ureq::tls::TlsConfig> {
    if let Some(cfg) = HTTPS_TLS_CONFIG.get() {
        return Ok(cfg.clone());
    }
    let pem = std::fs::read("/etc/ssl/cert.pem").context("read /etc/ssl/cert.pem")?;
    let mut certs = Vec::new();
    for item in ureq::tls::parse_pem(&pem) {
        if let Ok(ureq::tls::PemItem::Certificate(cert)) = item {
            certs.push(cert);
        }
    }
    ensure!(
        !certs.is_empty(),
        "no usable certificates in /etc/ssl/cert.pem"
    );
    let tls = ureq::tls::TlsConfig::builder()
        .provider(ureq::tls::TlsProvider::Rustls)
        .root_certs(ureq::tls::RootCerts::new_with_certs(&certs))
        .unversioned_rustls_crypto_provider(Arc::new(rustls::crypto::ring::default_provider()))
        .build();
    let _ = HTTPS_TLS_CONFIG.set(tls.clone());
    Ok(tls)
}

fn https_agent() -> Result<ureq::Agent> {
    let tls = load_https_tls()?;
    let config = ureq::Agent::config_builder()
        .https_only(true)
        .max_redirects(0)
        .timeout_resolve(Some(RESOLVE_TIMEOUT))
        .timeout_connect(Some(CONNECT_TIMEOUT))
        .timeout_send_request(Some(SEND_TIMEOUT))
        .timeout_recv_response(Some(RESPONSE_TIMEOUT))
        .timeout_per_read(Some(PER_IO_TIMEOUT))
        .timeout_per_write(Some(PER_IO_TIMEOUT))
        .tls_config(tls)
        .build();
    Ok(ureq::Agent::new_with_config(config))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn enforces_http_policy_for_primary_and_mirrors() {
        Repo::new("file:///tmp/repo").unwrap();
        Repo::new("http://127.0.0.1:8080").unwrap();
        Repo::new("http://[::1]:8080/kl").unwrap();
        Repo::new("http://localhost:9000").unwrap();

        for bad_primary in [
            "http://kl.kido.sh",
            "http://10.0.0.5:8080",
            "http://127.0.0.1.evil.com",
            "http://127.0.0.1@evil.com",
            "http://[::ffff:8.8.8.8]:8080",
            "ftp://kl.kido.sh",
            "file://relative/path",
            "file:///tmp/../etc",
            "http://127.0.0.1:8080?x=1",
        ] {
            assert!(
                Repo::new(bad_primary).is_err(),
                "primary {bad_primary:?} was accepted"
            );
        }

        for ok_mirror in [
            "file:///tmp/mirror",
            "http://127.0.0.1:8080",
            "http://10.20.30.40:8080/kl",
            "http://192.168.1.50/kl",
            "http://172.16.0.9:8080",
            "http://[fd00::1]:8080",
            "http://kl-cache.local:8080",
            "http://kl-mirror.internal",
            "http://kl-cache:8080",
        ] {
            Repo::new_mirror(ok_mirror).unwrap();
        }

        for bad_mirror in [
            "http://kl.kido.sh",
            "http://8.8.8.8/kl",
            "http://169.254.169.254/latest",
            "http://[::ffff:8.8.8.8]:8080",
            "http://134744072",
            "http://0x08080808",
            "http://01002004010",
            "http://mirror.example.com",
            "http://10.0.0.1@evil.com",
            "http://127.0.0.1.evil.com",
        ] {
            assert!(
                Repo::new_mirror(bad_mirror).is_err(),
                "mirror {bad_mirror:?} was accepted"
            );
        }

        // Primary URL policy must NOT be bypassed by duplicating primary in mirrors.
        assert!(RepoSet::new(&["http://10.0.0.5:8080".into()], "http://10.0.0.5:8080").is_err());
    }

    #[test]
    fn rejects_traversal_in_repo_rel_paths() {
        check_repo_rel("v1/26/1/1/24B56/latest/timestamp.json").unwrap();
        check_repo_rel("v1/blobs/sha256/aaaa").unwrap();
        for bad in [
            "",
            "/v1/timestamp.json",
            "../secret",
            "v1/../secret",
            "v1/./timestamp.json",
            "v1//timestamp.json",
            "v1/timestamp.json?foo=1",
            "v1/timestamp.json#frag",
        ] {
            assert!(check_repo_rel(bad).is_err(), "{bad:?} was accepted");
        }
    }
}
