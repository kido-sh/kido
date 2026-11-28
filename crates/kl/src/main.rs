use anyhow::{Result, anyhow, bail, ensure};
use clap::{Parser, Subcommand};
use kido_trust::{RepoTarget, Timestamp, TrustedKeys, verify_timestamp};
use kl::config::{EffectiveConfig, Overrides};
use kl::db::{Db, read_file_nofollow};
use kl::index::Index;
use kl::install::{self, closure, installed_index};
use kl::manifest::Kind;
use kl::repo::{Repo, RepoSet};
use std::path::PathBuf;
use std::process::{Command, ExitCode};

const EMBEDDED_KEYS: &str = include_str!("../keys/trusted.pub");
const MAX_KEYS_FILE_SIZE: u64 = 64 << 10;
const MAX_INDEX_SIZE: u64 = 64 << 20;

#[derive(Parser)]
#[command(
    name = "kl",
    version,
    about = "Install the system libraries and frameworks of KidoDarwin"
)]
struct Cli {
    #[arg(long, global = true, default_value = "/")]
    root: PathBuf,
    #[arg(
        long,
        global = true,
        help = "Primary repository URL (default: https://kl.kido.sh)"
    )]
    repo: Option<String>,
    #[arg(
        long = "mirror",
        global = true,
        help = "Untrusted mirror URL tried before --repo (repeatable)"
    )]
    mirrors: Vec<String>,
    #[arg(long, global = true, help = "Repository schema version (default: v1)")]
    kl_version: Option<String>,
    #[arg(
        long,
        global = true,
        help = "OS version X.Y or X.Y.Z (default: SystemVersion.plist)"
    )]
    os_version: Option<String>,
    #[arg(
        long,
        global = true,
        help = "OS build string (default: SystemVersion.plist)"
    )]
    os_build: Option<String>,
    #[arg(
        long,
        global = true,
        help = "Repository channel: latest or nightly (default: latest)"
    )]
    channel: Option<String>,
    #[arg(long, global = true, help = "Target architecture: arm64 or arm64e")]
    arch: Option<String>,
    #[arg(long, global = true)]
    keys: Option<PathBuf>,
    #[command(subcommand)]
    cmd: Cmd,
}

#[derive(Subcommand)]
enum Cmd {
    Add {
        #[arg(required = true)]
        names: Vec<String>,
    },
    Rm {
        #[arg(required = true)]
        names: Vec<String>,
    },
    List,
    Search {
        term: String,
    },
    Info {
        name: String,
    },
    Which {
        path: String,
    },
    Verify {
        name: Option<String>,
    },
    Update,
}

impl Cmd {
    fn is_mutating(&self) -> bool {
        matches!(self, Cmd::Add { .. } | Cmd::Rm { .. } | Cmd::Update)
    }
}

struct Fetched {
    repos: RepoSet,
    target: RepoTarget,
    index: Index,
    version: u64,
}

fn main() -> ExitCode {
    match run() {
        Ok(code) => code,
        Err(e) => {
            eprintln!("kl: {e:#}");
            ExitCode::FAILURE
        }
    }
}

fn run() -> Result<ExitCode> {
    let cli = Cli::parse();
    let overrides = Overrides {
        repo: cli.repo.clone(),
        mirrors: cli.mirrors.clone(),
        kl_version: cli.kl_version.clone(),
        os_version: cli.os_version.clone(),
        os_build: cli.os_build.clone(),
        channel: cli.channel.clone(),
        arch: cli.arch.clone(),
    };
    let cfg = EffectiveConfig::resolve(&cli.root, &overrides, host_arch)?;
    let db = open_db_and_recover(&cli.root, cli.cmd.is_mutating())?;
    match &cli.cmd {
        Cmd::Add { names } => add(&cli, &cfg, &db, names),
        Cmd::Rm { names } => rm(&cfg.arch, &db, names),
        Cmd::List => list(&db),
        Cmd::Search { term } => search(&cli, &cfg, &db, term),
        Cmd::Info { name } => info(&cli, &cfg, &db, name),
        Cmd::Which { path } => which(&cli, &cfg, &db, path),
        Cmd::Verify { name } => verify(&db, name.as_deref()),
        Cmd::Update => update(&cli, &cfg, &db),
    }
}

fn open_db_and_recover(root: &std::path::Path, mutating: bool) -> Result<Db> {
    if mutating {
        let db = Db::open_rw(root)?;
        if install::recover(&db)? {
            eprintln!("kl: finished an interrupted transaction");
        }
        return Ok(db);
    }

    let db = Db::open_readonly(root)?;
    if db.has_journal() {
        // Release shared lock before upgrading to exclusive lock to finish an interrupted journal.
        drop(db);
        match Db::open_rw(root) {
            Ok(rw) => {
                if install::recover(&rw)? {
                    eprintln!("kl: finished an interrupted transaction");
                }
                drop(rw);
                return Db::open_readonly(root);
            }
            Err(e) => {
                eprintln!(
                    "kl: warning: interrupted transaction pending and root is not writable ({e:#})"
                );
                return Db::open_readonly(root);
            }
        }
    }
    Ok(db)
}

fn add(cli: &Cli, cfg: &EffectiveConfig, db: &Db, names: &[String]) -> Result<ExitCode> {
    let fetched = fetch(cli, cfg, db)?;
    let mut world = db.world()?;
    for n in names {
        ensure!(fetched.index.find(n).is_some(), "no package named {n}");
        world.insert(n.clone());
    }
    let wanted = closure(&fetched.index, &db.root, &world)?;
    let seen = Some((fetched.target.state_key()?, fetched.version));
    let sources = fetched.repos.mirrors_then_primary();
    for w in install::sync(
        db,
        Some(&sources),
        Some(&fetched.target),
        &cfg.arch,
        &wanted,
        world,
        seen,
    )? {
        eprintln!("kl: {w}");
    }
    Ok(ExitCode::SUCCESS)
}

fn rm(arch: &str, db: &Db, names: &[String]) -> Result<ExitCode> {
    let installed = db.installed()?;
    let mut world = db.world()?;
    for n in names {
        ensure!(installed.contains_key(n), "{n} is not installed");
        world.remove(n);
    }
    let view = installed_index(&installed, arch);
    let wanted = closure(&view, &db.root, &world)?;
    for n in names.iter().filter(|n| wanted.contains_key(*n)) {
        eprintln!("kl: {n} stays installed because another package needs it");
    }
    for w in install::sync(db, None, None, arch, &wanted, world, None)? {
        eprintln!("kl: {w}");
    }
    Ok(ExitCode::SUCCESS)
}

fn list(db: &Db) -> Result<ExitCode> {
    let world = db.world()?;
    for (name, i) in db.installed()? {
        let note = if world.contains(&name) {
            ""
        } else {
            " (dependency)"
        };
        println!("{name} {}{note}", i.manifest.version);
    }
    Ok(ExitCode::SUCCESS)
}

fn search(cli: &Cli, cfg: &EffectiveConfig, db: &Db, term: &str) -> Result<ExitCode> {
    let fetched = fetch(cli, cfg, db)?;
    let term = term.to_lowercase();
    for p in fetched.index.packages.iter().filter(|p| {
        p.name.to_lowercase().contains(&term) || p.summary.to_lowercase().contains(&term)
    }) {
        println!("{} {}: {}", p.name, p.version, p.summary);
    }
    Ok(ExitCode::SUCCESS)
}

fn info(cli: &Cli, cfg: &EffectiveConfig, db: &Db, name: &str) -> Result<ExitCode> {
    let fetched = fetch(cli, cfg, db)?;
    let p = fetched
        .index
        .find(name)
        .ok_or_else(|| anyhow!("no package named {name}"))?;
    let installed = db
        .installed()?
        .get(name)
        .map(|i| i.manifest.version.clone());
    println!(
        "name: {}\nversion: {}\nsummary: {}\nlicense: {}",
        p.name, p.version, p.summary, p.license
    );
    println!(
        "size: {} bytes, {} installed",
        p.blob.size, p.installed_size
    );
    println!(
        "provides: {}\ndepends: {}",
        p.provides.join(" "),
        p.depends.join(" ")
    );
    println!("installed: {}", installed.as_deref().unwrap_or("no"));
    Ok(ExitCode::SUCCESS)
}

fn verify(db: &Db, name: Option<&str>) -> Result<ExitCode> {
    let installed = db.installed()?;
    if let Some(n) = name {
        ensure!(installed.contains_key(n), "{n} is not installed");
    }
    let mut bad = 0;
    for (n, i) in installed
        .iter()
        .filter(|(n, _)| name.is_none_or(|x| x == n.as_str()))
    {
        for problem in install::verify(&db.root, i) {
            println!("{n}: {problem}");
            bad += 1;
        }
    }
    Ok(if bad > 0 {
        ExitCode::FAILURE
    } else {
        ExitCode::SUCCESS
    })
}

fn update(cli: &Cli, cfg: &EffectiveConfig, db: &Db) -> Result<ExitCode> {
    let fetched = fetch(cli, cfg, db)?;
    let mut state = db.state()?;
    let seen = state.seen.entry(fetched.target.state_key()?).or_default();
    *seen = (*seen).max(fetched.version);
    db.put_state(&state)?;
    println!(
        "{} {}: version {}, {} packages",
        fetched.target,
        cfg.arch,
        fetched.version,
        fetched.index.packages.len()
    );
    Ok(ExitCode::SUCCESS)
}

fn fetch(cli: &Cli, cfg: &EffectiveConfig, db: &Db) -> Result<Fetched> {
    let keys = match &cli.keys {
        Some(path) => {
            let bytes = read_file_nofollow(path, MAX_KEYS_FILE_SIZE)?
                .ok_or_else(|| anyhow!("keys file {} not found", path.display()))?;
            let text = String::from_utf8(bytes)
                .map_err(|_| anyhow!("keys file {} is not UTF-8", path.display()))?;
            TrustedKeys::parse(&text)?
        }
        None => TrustedKeys::parse(EMBEDDED_KEYS)?,
    };
    ensure!(
        !keys.is_empty(),
        "this kl has no trusted keys built in; pass --keys"
    );
    let target = cfg.resolve_target(&db.root)?;
    let repos = RepoSet::new(&cfg.mirrors, &cfg.repo)?;
    let state_key = target.state_key()?;
    let seen = db.state()?.seen.get(&state_key).copied().unwrap_or(0);
    let now = time::OffsetDateTime::now_utc();

    let ts = fetch_authoritative_timestamp(&repos, &keys, &target, now, seen)?;
    let blob = ts
        .indexes
        .get(&cfg.arch)
        .ok_or_else(|| anyhow!("{target} has no {} index", cfg.arch))?;
    ensure!(
        blob.size <= MAX_INDEX_SIZE,
        "{target} {} index size {} exceeds limit {MAX_INDEX_SIZE}",
        cfg.arch,
        blob.size
    );
    let index_rel = target.index_path(&cfg.arch, &blob.sha256)?;

    // Fetch content-addressed index from mirrors first, falling back to primary repo.
    let sources = repos.mirrors_then_primary();
    let total = sources.len();
    for (idx, repo) in sources.iter().enumerate() {
        let attempt = (|| -> Result<Index> {
            let bytes = repo.read(&index_rel, blob.size)?;
            blob.check(&bytes)?;
            Index::parse(&bytes, &target, &cfg.arch)
        })();
        match attempt {
            Ok(index) => {
                return Ok(Fetched {
                    repos,
                    target,
                    index,
                    version: ts.version,
                });
            }
            Err(e) => {
                if idx + 1 < total {
                    eprintln!(
                        "kl: warning: mirror {} failed for index ({e:#}); trying next source",
                        repo.url()
                    );
                } else {
                    return Err(e);
                }
            }
        }
    }
    bail!("no repository source available")
}

/// Anchors freshness to the primary repository's tiny `timestamp.json` when reachable so a
/// malicious mirror cannot freeze or downgrade a fresh client. If the primary repository is
/// unreachable, falls back to untrusted mirrors and selects the highest cryptographically
/// verified timestamp version across all reachable mirrors.
fn fetch_authoritative_timestamp(
    repos: &RepoSet,
    keys: &TrustedKeys,
    target: &RepoTarget,
    now: time::OffsetDateTime,
    seen: u64,
) -> Result<Timestamp> {
    let primary_err = match fetch_timestamp_from(repos.primary(), keys, target, now, seen) {
        Ok(ts) => return Ok(ts),
        Err(e) => e,
    };

    if repos.mirrors().is_empty() {
        return Err(primary_err);
    }

    eprintln!(
        "kl: warning: primary repository {} unavailable ({primary_err:#}); trying mirrors",
        repos.primary().url()
    );

    let mut best: Option<Timestamp> = None;
    for mirror in repos.mirrors() {
        match fetch_timestamp_from(mirror, keys, target, now, seen) {
            Ok(ts) => {
                if best.as_ref().is_none_or(|b| ts.version > b.version) {
                    best = Some(ts);
                }
            }
            Err(e) => {
                eprintln!(
                    "kl: warning: mirror {} failed ({e:#}); trying next source",
                    mirror.url()
                );
            }
        }
    }
    best.ok_or(primary_err)
}

fn fetch_timestamp_from(
    repo: &Repo,
    keys: &TrustedKeys,
    target: &RepoTarget,
    now: time::OffsetDateTime,
    seen: u64,
) -> Result<Timestamp> {
    let stamp = repo.read(&target.timestamp_path()?, 1 << 20)?;
    let sig = repo.read(&target.timestamp_sig_path()?, 64 << 10)?;
    Ok(verify_timestamp(
        keys,
        kl::NAMESPACE,
        &stamp,
        &sig,
        target,
        now,
        seen,
    )?)
}

fn which(cli: &Cli, cfg: &EffectiveConfig, db: &Db, path: &str) -> Result<ExitCode> {
    let rel = path.trim_start_matches('/').trim_end_matches('/');
    ensure!(!rel.is_empty(), "bad path {path:?}");
    let dylib_key = format!("dylib:/{rel}");
    let installed = db.installed()?;
    for i in installed.values() {
        if i.manifest
            .files
            .iter()
            .any(|f| f.kind != Kind::Dir && f.path == rel)
        {
            println!("{path}: {}", i.manifest.name);
            return Ok(ExitCode::SUCCESS);
        }
    }
    for i in installed.values() {
        if i.manifest.provides.iter().any(|x| x == &dylib_key)
            || i.manifest.files.iter().any(|f| f.path == rel)
        {
            println!("{path}: {}", i.manifest.name);
            return Ok(ExitCode::SUCCESS);
        }
    }
    let fetched = fetch(cli, cfg, db)?;
    if let Some(p) = fetched
        .index
        .owner_of(rel)
        .or_else(|| fetched.index.provider(&dylib_key))
    {
        println!(
            "{path}: not installed; it comes with {} (kl add {})",
            p.name, p.name
        );
        return Ok(ExitCode::SUCCESS);
    }
    bail!("no package provides {path}")
}

fn host_arch() -> String {
    for sysctl in ["/usr/sbin/sysctl", "/sbin/sysctl", "/bin/sysctl"] {
        if let Ok(out) = Command::new(sysctl)
            .args(["-n", "hw.optional.arm.FEAT_PAuth"])
            .output()
        {
            return if String::from_utf8_lossy(&out.stdout).trim() == "1" {
                "arm64e"
            } else {
                "arm64"
            }
            .to_owned();
        }
    }
    "arm64".to_owned()
}
