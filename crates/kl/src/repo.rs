use anyhow::{Context, Result, bail, ensure};
use std::fs::File;
use std::io::Read;
use std::path::PathBuf;
use std::sync::Arc;

pub struct Repo {
    base: String,
    agent: Option<ureq::Agent>,
}

impl Repo {
    pub fn new(base: &str) -> Result<Self> {
        let base = base.trim_end_matches('/').to_owned();
        if base.starts_with("file://") {
            Ok(Repo { base, agent: None })
        } else if base.starts_with("https://") {
            Ok(Repo {
                base,
                agent: Some(https_agent()?),
            })
        } else {
            bail!("the repository must be an https:// or file:// URL, not {base}")
        }
    }

    pub fn open(&self, rel: &str) -> Result<Box<dyn Read>> {
        match &self.agent {
            None => {
                let path = PathBuf::from(&self.base["file://".len()..]).join(rel);
                Ok(Box::new(
                    File::open(&path).with_context(|| format!("open {}", path.display()))?,
                ))
            }
            Some(agent) => {
                let url = format!("{}/{rel}", self.base);
                let response = agent
                    .get(&url)
                    .call()
                    .with_context(|| format!("GET {url}"))?;
                Ok(Box::new(response.into_reader()))
            }
        }
    }

    pub fn read(&self, rel: &str, limit: u64) -> Result<Vec<u8>> {
        let mut buf = Vec::new();
        self.open(rel)?.take(limit + 1).read_to_end(&mut buf)?;
        ensure!(
            buf.len() as u64 <= limit,
            "{rel} is larger than {limit} bytes"
        );
        Ok(buf)
    }
}

fn https_agent() -> Result<ureq::Agent> {
    let pem = std::fs::read("/etc/ssl/cert.pem").context("read /etc/ssl/cert.pem")?;
    let certs = rustls_pemfile::certs(&mut pem.as_slice()).collect::<Result<Vec<_>, _>>()?;
    let mut roots = rustls::RootCertStore::empty();
    let (added, _) = roots.add_parsable_certificates(certs);
    ensure!(added > 0, "no usable certificates in /etc/ssl/cert.pem");
    let config = rustls::ClientConfig::builder_with_provider(Arc::new(
        rustls::crypto::ring::default_provider(),
    ))
    .with_safe_default_protocol_versions()?
    .with_root_certificates(roots)
    .with_no_client_auth();
    Ok(ureq::AgentBuilder::new()
        .tls_config(Arc::new(config))
        .build())
}
