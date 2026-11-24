use anyhow::{Result, anyhow, bail, ensure};
use clap::{Parser, Subcommand};
use kido_trust::{TrustedKeys, verify_timestamp};
use kl::db::Db;
use kl::index::Index;
use kl::install::{self, closure, installed_index};
use kl::repo::Repo;
use std::path::PathBuf;
use std::process::{Command, ExitCode};

const EMBEDDED_KEYS: &str = include_str!("../keys/trusted.pub");

#[derive(Parser)]
#[command(
    name = "kl",
    version,
    about = "Install the system libraries and frameworks of KidoDarwin"
)]
struct Cli {
    #[arg(long, global = true, default_value = "/")]
    root: PathBuf,
    #[arg(long, global = true, default_value = "https://kl.kido.sh")]
    repo: String,
    #[arg(long, global = true, default_value = "latest")]
    channel: String,
    #[arg(long, global = true)]
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
    kl::check_channel(&cli.channel)?;
    let arch = cli.arch.clone().unwrap_or_else(host_arch);
    kl::check_arch(&arch)?;
    let db = Db::open(&cli.root)?;
    if install::recover(&db)? {
        eprintln!("kl: finished an interrupted transaction");
    }
    match &cli.cmd {
        Cmd::Add { names } => add(&cli, &arch, &db, names),
        Cmd::Rm { names } => rm(&arch, &db, names),
        Cmd::List => list(&db),
        Cmd::Search { term } => search(&cli, &arch, &db, term),
        Cmd::Info { name } => info(&cli, &arch, &db, name),
        Cmd::Which { path } => which(&cli, &arch, &db, path),
        Cmd::Verify { name } => verify(&db, name.as_deref()),
        Cmd::Update => update(&cli, &arch, &db),
    }
}

fn add(cli: &Cli, arch: &str, db: &Db, names: &[String]) -> Result<ExitCode> {
    let (repo, index, version) = fetch(cli, arch, db)?;
    let mut world = db.world()?;
    for n in names {
        ensure!(index.find(n).is_some(), "no package named {n}");
        world.insert(n.clone());
    }
    let wanted = closure(&index, &db.root, &world)?;
    let seen = Some((cli.channel.clone(), version));
    for w in install::sync(db, Some(&repo), arch, &wanted, world, seen)? {
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
    for w in install::sync(db, None, arch, &wanted, world, None)? {
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

fn search(cli: &Cli, arch: &str, db: &Db, term: &str) -> Result<ExitCode> {
    let (_, index, _) = fetch(cli, arch, db)?;
    let term = term.to_lowercase();
    for p in index.packages.iter().filter(|p| {
        p.name.to_lowercase().contains(&term) || p.summary.to_lowercase().contains(&term)
    }) {
        println!("{} {}: {}", p.name, p.version, p.summary);
    }
    Ok(ExitCode::SUCCESS)
}

fn info(cli: &Cli, arch: &str, db: &Db, name: &str) -> Result<ExitCode> {
    let (_, index, _) = fetch(cli, arch, db)?;
    let p = index
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

fn update(cli: &Cli, arch: &str, db: &Db) -> Result<ExitCode> {
    let (_, index, version) = fetch(cli, arch, db)?;
    let mut state = db.state()?;
    let seen = state.seen.entry(cli.channel.clone()).or_default();
    *seen = (*seen).max(version);
    db.put_state(&state)?;
    println!(
        "{} {arch}: version {version}, {} packages",
        cli.channel,
        index.packages.len()
    );
    Ok(ExitCode::SUCCESS)
}

fn fetch(cli: &Cli, arch: &str, db: &Db) -> Result<(Repo, Index, u64)> {
    let keys = match &cli.keys {
        Some(path) => TrustedKeys::parse(&std::fs::read_to_string(path)?)?,
        None => TrustedKeys::parse(EMBEDDED_KEYS)?,
    };
    ensure!(
        !keys.is_empty(),
        "this kl has no trusted keys built in; pass --keys"
    );
    let repo = Repo::new(&cli.repo)?;
    let stamp = repo.read(&format!("{}/timestamp.json", cli.channel), 1 << 20)?;
    let sig = repo.read(&format!("{}/timestamp.json.sig", cli.channel), 64 << 10)?;
    let seen = db.state()?.seen.get(&cli.channel).copied().unwrap_or(0);
    let ts = verify_timestamp(
        &keys,
        kl::NAMESPACE,
        &stamp,
        &sig,
        &cli.channel,
        time::OffsetDateTime::now_utc(),
        seen,
    )?;
    let blob = ts
        .indexes
        .get(arch)
        .ok_or_else(|| anyhow!("the {} channel has no {arch} index", cli.channel))?;
    let bytes = repo.read(
        &format!("{}/{arch}/index.{}.json", cli.channel, blob.sha256),
        64 << 20,
    )?;
    blob.check(&bytes)?;
    Ok((repo, Index::parse(&bytes, arch)?, ts.version))
}

fn which(cli: &Cli, arch: &str, db: &Db, path: &str) -> Result<ExitCode> {
    let rel = path.trim_start_matches('/');
    for i in db.installed()?.values() {
        if i.manifest.files.iter().any(|f| f.path == rel) {
            println!("{path}: {}", i.manifest.name);
            return Ok(ExitCode::SUCCESS);
        }
    }
    let (_, index, _) = fetch(cli, arch, db)?;
    if let Some(p) = index.provider(&format!("dylib:/{rel}")) {
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
