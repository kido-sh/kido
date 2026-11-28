use kido_trust::{OsVersion, RepoTarget};
use kl::db::{Db, Installed};
use kl::install::{Journal, Op};
use kl::manifest::{FileEntry, Kind, Manifest};
use std::fs;
use std::io::{BufRead, BufReader, Write};
use std::net::{TcpListener, TcpStream};
use std::os::unix::fs::{PermissionsExt, symlink};
use std::path::{Path, PathBuf};
use std::process::{Command, Output};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};
use std::thread;
use std::time::Duration;

const DEFAULT_OS: &str = "26.1.1";
const DEFAULT_BUILD: &str = "24B56";
const DEFAULT_ARCH: &str = "arm64e";

struct World {
    dir: tempfile::TempDir,
}

impl World {
    fn new() -> Self {
        let w = World {
            dir: tempfile::tempdir().unwrap(),
        };
        w.keygen("signer");
        w
    }

    fn path(&self, rel: &str) -> PathBuf {
        self.dir.path().join(rel)
    }

    fn keygen(&self, name: &str) -> PathBuf {
        let key = self.path(name);
        let ok = Command::new("ssh-keygen")
            .args(["-q", "-t", "ed25519", "-N", "", "-C", "test", "-f"])
            .arg(&key)
            .status()
            .unwrap()
            .success();
        assert!(ok, "ssh-keygen failed");
        key
    }

    fn repo_in(repo_dir: &Path, os_ver: &str, build: &str, channel: &str, args: &[&str]) -> Output {
        let out = Command::new(env!("CARGO_BIN_EXE_kl-repo"))
            .arg("--repo")
            .arg(repo_dir)
            .arg("--os-version")
            .arg(os_ver)
            .arg("--os-build")
            .arg(build)
            .arg("--channel")
            .arg(channel)
            .args(args)
            .output()
            .unwrap();
        assert!(
            out.status.success(),
            "kl-repo {args:?}: {}",
            String::from_utf8_lossy(&out.stderr)
        );
        out
    }

    fn repo(&self, args: &[&str]) -> Output {
        Self::repo_in(
            &self.path("repo"),
            DEFAULT_OS,
            DEFAULT_BUILD,
            "latest",
            args,
        )
    }

    fn kl_raw(&self, args: &[&str]) -> Output {
        Command::new(env!("CARGO_BIN_EXE_kl"))
            .arg("--root")
            .arg(self.path("root"))
            .args(args)
            .output()
            .unwrap()
    }

    fn kl_with(&self, keys: &str, args: &[&str]) -> Output {
        Command::new(env!("CARGO_BIN_EXE_kl"))
            .arg("--root")
            .arg(self.path("root"))
            .arg("--repo")
            .arg(format!("file://{}", self.path("repo").display()))
            .arg("--os-version")
            .arg(DEFAULT_OS)
            .arg("--os-build")
            .arg(DEFAULT_BUILD)
            .arg("--arch")
            .arg(DEFAULT_ARCH)
            .arg("--keys")
            .arg(self.path(keys))
            .args(args)
            .output()
            .unwrap()
    }

    fn kl(&self, args: &[&str]) -> Output {
        self.kl_with("signer.pub", args)
    }

    fn tree(&self) -> PathBuf {
        let from = self.path("tree");
        fs::create_dir_all(from.join("usr/lib")).unwrap();
        fs::create_dir_all(from.join("usr/bin")).unwrap();
        fs::create_dir_all(from.join("usr/share/foo/doc")).unwrap();
        fs::write(from.join("usr/lib/libfoo.dylib"), b"not really a dylib").unwrap();
        let _ = fs::remove_file(from.join("usr/lib/libfoo.1.dylib"));
        symlink("libfoo.dylib", from.join("usr/lib/libfoo.1.dylib")).unwrap();
        fs::write(from.join("usr/share/foo/data.txt"), b"foo data\n").unwrap();
        fs::write(from.join("usr/share/foo/doc/README"), b"read me\n").unwrap();
        fs::write(from.join("usr/lib/libbar.dylib"), b"bar").unwrap();
        fs::write(from.join("usr/bin/python3"), b"#!/bin/sh\necho py\n").unwrap();
        from
    }

    fn publish_in(
        &self,
        repo_dir: &Path,
        target: (&str, &str, &str),
        version: u64,
        expires: Option<&str>,
        key: &str,
    ) {
        let (os_ver, build, channel) = target;
        let v = version.to_string();
        Self::repo_in(
            repo_dir,
            os_ver,
            build,
            channel,
            &["index", "--arch", DEFAULT_ARCH, "--version", &v],
        );
        let mut ts = vec!["timestamp", "--version", &v];
        if let Some(e) = expires {
            ts.extend(["--expires", e]);
        }
        Self::repo_in(repo_dir, os_ver, build, channel, &ts);
        let key_path = self.path(key);
        Self::repo_in(
            repo_dir,
            os_ver,
            build,
            channel,
            &["sign", "--key", key_path.to_str().unwrap()],
        );
    }

    fn publish(&self, version: u64, expires: Option<&str>, key: &str) {
        self.publish_in(
            &self.path("repo"),
            (DEFAULT_OS, DEFAULT_BUILD, "latest"),
            version,
            expires,
            key,
        );
    }

    fn pack_both_in(&self, repo_dir: &Path, os_ver: &str, build: &str, channel: &str) {
        let from = self.tree();
        let from = from.to_str().unwrap();
        Self::repo_in(
            repo_dir,
            os_ver,
            build,
            channel,
            &[
                "pack",
                "--arch",
                DEFAULT_ARCH,
                "--from",
                from,
                "--name",
                "foo",
                "--version",
                "1.0",
                "--license",
                "MIT",
                "--summary",
                "the foo library",
                "usr/lib/libfoo.dylib",
                "usr/lib/libfoo.1.dylib",
                "usr/share/foo",
            ],
        );
        Self::repo_in(
            repo_dir,
            os_ver,
            build,
            channel,
            &[
                "pack",
                "--arch",
                DEFAULT_ARCH,
                "--from",
                from,
                "--name",
                "bar",
                "--version",
                "2.0",
                "--license",
                "MIT",
                "--summary",
                "bar, which needs foo",
                "--depend",
                "foo",
                "usr/lib/libbar.dylib",
            ],
        );
    }

    fn pack_both(&self) {
        self.pack_both_in(&self.path("repo"), DEFAULT_OS, DEFAULT_BUILD, "latest");
    }
}

struct HttpRepoServer {
    url: String,
    requests: Arc<Mutex<Vec<String>>>,
    stop: Arc<AtomicBool>,
    handle: Option<thread::JoinHandle<()>>,
}

#[derive(Clone, Copy)]
enum ServerMode {
    Files,
    Status503,
    Redirect302,
}

impl HttpRepoServer {
    fn start(root: PathBuf) -> Self {
        Self::start_with_mode(root, ServerMode::Files)
    }

    fn start_error() -> Self {
        Self::start_with_mode(PathBuf::new(), ServerMode::Status503)
    }

    fn start_redirect() -> Self {
        Self::start_with_mode(PathBuf::new(), ServerMode::Redirect302)
    }

    fn start_with_mode(root: PathBuf, mode: ServerMode) -> Self {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let addr = listener.local_addr().unwrap();
        listener.set_nonblocking(true).unwrap();
        let requests = Arc::new(Mutex::new(Vec::new()));
        let reqs_bg = Arc::clone(&requests);
        let stop = Arc::new(AtomicBool::new(false));
        let stop_bg = Arc::clone(&stop);

        let handle = thread::spawn(move || {
            while !stop_bg.load(Ordering::Relaxed) {
                match listener.accept() {
                    Ok((stream, _)) => {
                        let _ = stream.set_nonblocking(false);
                        let root_c = root.clone();
                        let reqs_c = Arc::clone(&reqs_bg);
                        thread::spawn(move || {
                            handle_http_conn(stream, &root_c, &reqs_c, mode);
                        });
                    }
                    Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => {
                        thread::sleep(Duration::from_millis(5));
                    }
                    Err(_) => break,
                }
            }
        });

        HttpRepoServer {
            url: format!("http://{addr}"),
            requests,
            stop,
            handle: Some(handle),
        }
    }

    fn recorded_paths(&self) -> Vec<String> {
        self.requests.lock().unwrap().clone()
    }
}

impl Drop for HttpRepoServer {
    fn drop(&mut self) {
        self.stop.store(true, Ordering::Relaxed);
        if let Some(h) = self.handle.take() {
            let _ = h.join();
        }
    }
}

fn handle_http_conn(
    mut stream: TcpStream,
    root: &Path,
    requests: &Arc<Mutex<Vec<String>>>,
    mode: ServerMode,
) {
    let _ = stream.set_read_timeout(Some(Duration::from_secs(5)));
    let mut reader = BufReader::new(stream.try_clone().unwrap());
    let mut request_line = String::new();
    if reader.read_line(&mut request_line).is_err() || request_line.is_empty() {
        return;
    }
    loop {
        let mut header = String::new();
        if reader.read_line(&mut header).is_err()
            || header == "\r\n"
            || header == "\n"
            || header.is_empty()
        {
            break;
        }
    }
    let req_path = request_line
        .split_whitespace()
        .nth(1)
        .unwrap_or("/")
        .to_owned();
    requests.lock().unwrap().push(req_path.clone());

    match mode {
        ServerMode::Status503 => {
            let _ = write!(
                stream,
                "HTTP/1.1 503 Service Unavailable\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"
            );
            return;
        }
        ServerMode::Redirect302 => {
            let _ = write!(
                stream,
                "HTTP/1.1 302 Found\r\nLocation: http://evil.example/pwn\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"
            );
            return;
        }
        ServerMode::Files => {}
    }

    let rel = req_path.trim_start_matches('/');
    if rel.is_empty()
        || rel
            .split('/')
            .any(|c| c.is_empty() || c == "." || c == "..")
    {
        let _ = write!(
            stream,
            "HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"
        );
        return;
    }
    let file_path = root.join(rel);
    match fs::read(&file_path) {
        Ok(body) => {
            let header = format!(
                "HTTP/1.1 200 OK\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
                body.len()
            );
            let _ = stream.write_all(header.as_bytes());
            let _ = stream.write_all(&body);
        }
        Err(_) => {
            let _ = write!(
                stream,
                "HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"
            );
        }
    }
}

fn copy_dir_all(src: &Path, dst: &Path) {
    fs::create_dir_all(dst).unwrap();
    for entry in fs::read_dir(src).unwrap() {
        let entry = entry.unwrap();
        let ty = entry.file_type().unwrap();
        let target = dst.join(entry.file_name());
        if ty.is_dir() {
            copy_dir_all(&entry.path(), &target);
        } else {
            fs::copy(entry.path(), target).unwrap();
        }
    }
}

fn ok(out: &Output) {
    assert!(
        out.status.success(),
        "stdout: {}\nstderr: {}",
        String::from_utf8_lossy(&out.stdout),
        String::from_utf8_lossy(&out.stderr)
    );
}

fn fails_with(out: &Output, text: &str) {
    let err = String::from_utf8_lossy(&out.stderr);
    assert!(
        !out.status.success(),
        "succeeded, expected an error containing {text:?}"
    );
    assert!(err.contains(text), "expected {text:?} in: {err}");
}

fn nothing_installed(root: &Path) {
    assert!(!root.join("usr").exists(), "files reached the root");
    let db = Db::open(root).unwrap();
    assert!(
        db.installed().unwrap().is_empty(),
        "the database lists packages"
    );
    assert!(!db.staging().exists(), "staging was left behind");
}

#[test]
fn installs_verifies_and_removes() {
    let w = World::new();
    w.pack_both();
    w.publish(1, None, "signer");
    ok(&w.kl(&["add", "bar"]));
    let root = w.path("root");
    assert_eq!(fs::read(root.join("usr/lib/libbar.dylib")).unwrap(), b"bar");
    assert_eq!(
        fs::read(root.join("usr/lib/libfoo.dylib")).unwrap(),
        b"not really a dylib"
    );
    assert_eq!(
        fs::read_link(root.join("usr/lib/libfoo.1.dylib")).unwrap(),
        Path::new("libfoo.dylib")
    );
    assert_eq!(
        fs::read(root.join("usr/share/foo/doc/README")).unwrap(),
        b"read me\n"
    );
    let list = String::from_utf8(w.kl(&["list"]).stdout).unwrap();
    assert_eq!(list, "bar 2.0\nfoo 1.0 (dependency)\n");
    ok(&w.kl(&["verify"]));
    let which = String::from_utf8(w.kl(&["which", "/usr/lib/libfoo.dylib"]).stdout).unwrap();
    assert_eq!(which, "/usr/lib/libfoo.dylib: foo\n");

    fs::write(root.join("usr/share/foo/data.txt"), b"changed by hand\n").unwrap();
    let verify = w.kl(&["verify"]);
    assert!(!verify.status.success());
    assert_eq!(
        String::from_utf8(verify.stdout).unwrap(),
        "foo: usr/share/foo/data.txt: changed\n"
    );

    let rm = w.kl(&["rm", "bar"]);
    ok(&rm);
    assert!(String::from_utf8_lossy(&rm.stderr).contains("kept usr/share/foo/data.txt"));
    assert!(!root.join("usr/lib/libbar.dylib").exists());
    assert!(!root.join("usr/lib/libfoo.dylib").exists());
    assert!(fs::symlink_metadata(root.join("usr/lib/libfoo.1.dylib")).is_err());
    assert!(!root.join("usr/share/foo/doc").exists());
    assert_eq!(
        fs::read(root.join("usr/share/foo/data.txt")).unwrap(),
        b"changed by hand\n"
    );
    assert!(Db::open(&root).unwrap().installed().unwrap().is_empty());
}

#[test]
fn refuses_a_tampered_package() {
    let w = World::new();
    w.pack_both();
    w.publish(1, None, "signer");
    let blobs = w.path("repo/v1/blobs/sha256");
    for entry in fs::read_dir(&blobs).unwrap() {
        let path = entry.unwrap().path();
        let mut bytes = fs::read(&path).unwrap();
        let last = bytes.len() - 1;
        bytes[last] ^= 0x55;
        fs::write(&path, bytes).unwrap();
    }
    let out = w.kl(&["add", "foo"]);
    assert!(!out.status.success());
    nothing_installed(&w.path("root"));
}

#[test]
fn refuses_a_package_that_unpacks_cleanly_but_has_another_hash() {
    let w = World::new();
    w.pack_both();
    w.publish(1, None, "signer");
    let entry = w.path("repo/v1/26/1/1/24B56/latest/arm64e/packages.d/foo.json");
    let mut package: kl::index::Package =
        serde_json::from_slice(&fs::read(&entry).unwrap()).unwrap();
    let blob = w.path("repo/v1/blobs/sha256").join(&package.blob.sha256);
    let plain = zstd::decode_all(fs::read(&blob).unwrap().as_slice()).unwrap();
    let other = zstd::encode_all(plain.as_slice(), 1).unwrap();
    package.blob.size = other.len() as u64;
    fs::write(&blob, other).unwrap();
    fs::write(&entry, serde_json::to_vec(&package).unwrap()).unwrap();

    // kl-repo index itself refuses to publish a corrupted blob!
    let bad_index = Command::new(env!("CARGO_BIN_EXE_kl-repo"))
        .args([
            "--repo",
            w.path("repo").to_str().unwrap(),
            "--os-version",
            DEFAULT_OS,
            "--os-build",
            DEFAULT_BUILD,
            "index",
            "--arch",
            DEFAULT_ARCH,
            "--version",
            "2",
        ])
        .output()
        .unwrap();
    assert!(
        !bad_index.status.success(),
        "kl-repo index should refuse corrupted blob on disk"
    );

    // Craft a signed index pointing to the recompressed blob (different hash) to test kl client.
    let target_dir = w.path("repo/v1/26/1/1/24B56/latest/arm64e");
    let target = RepoTarget::new(
        "v1",
        OsVersion::parse(DEFAULT_OS).unwrap(),
        DEFAULT_BUILD,
        "latest",
    )
    .unwrap();
    let idx = kl::index::Index {
        version: 2,
        target,
        arch: DEFAULT_ARCH.into(),
        packages: vec![package],
    };
    let idx_bytes = serde_json::to_vec_pretty(&idx).unwrap();
    let idx_ref = kido_trust::BlobRef::of(&idx_bytes);
    fs::write(
        target_dir.join(format!("index.{}.json", idx_ref.sha256)),
        &idx_bytes,
    )
    .unwrap();
    fs::write(
        target_dir.join("CURRENT"),
        serde_json::to_vec(&idx_ref).unwrap(),
    )
    .unwrap();
    w.repo(&["timestamp", "--version", "2"]);
    let key = w.path("signer");
    w.repo(&["sign", "--key", key.to_str().unwrap()]);

    fails_with(&w.kl(&["add", "foo"]), "does not match");
    nothing_installed(&w.path("root"));
}

fn pack_big(w: &World) -> kl::index::Package {
    let from = w.path("bigtree");
    fs::create_dir_all(from.join("usr/share/big")).unwrap();
    let mut x: u64 = 0x9e37_79b9_7f4a_7c15;
    let bytes: Vec<u8> = (0..9 << 20)
        .map(|_| {
            x ^= x << 13;
            x ^= x >> 7;
            x ^= x << 17;
            x.to_le_bytes()[0]
        })
        .collect();
    fs::write(from.join("usr/share/big/blob.bin"), bytes).unwrap();
    w.repo(&[
        "pack",
        "--arch",
        DEFAULT_ARCH,
        "--from",
        from.to_str().unwrap(),
        "--name",
        "big",
        "--version",
        "1",
        "--license",
        "MIT",
        "usr/share/big",
    ]);
    serde_json::from_slice(
        &fs::read(w.path("repo/v1/26/1/1/24B56/latest/arm64e/packages.d/big.json")).unwrap(),
    )
    .unwrap()
}

#[test]
fn installs_a_large_package_chunk_by_chunk() {
    let w = World::new();
    let entry = pack_big(&w);
    assert!(entry.blob.size > 2 * kido_trust::CHUNK_SIZE);
    assert!(
        entry.chunks.is_some(),
        "a large blob should list chunk hashes"
    );
    w.publish(1, None, "signer");
    ok(&w.kl(&["add", "big"]));
    assert_eq!(
        fs::read(w.path("root/usr/share/big/blob.bin"))
            .unwrap()
            .len(),
        9 << 20
    );
}

#[test]
fn stops_at_the_first_tampered_chunk() {
    let w = World::new();
    let entry = pack_big(&w);
    w.publish(1, None, "signer");
    let blob = w.path("repo/v1/blobs/sha256").join(&entry.blob.sha256);
    let mut bytes = fs::read(&blob).unwrap();
    bytes[usize::try_from(kido_trust::CHUNK_SIZE).unwrap() + 10] ^= 1;
    fs::write(&blob, bytes).unwrap();
    fails_with(&w.kl(&["add", "big"]), "chunk 1 of the blob does not match");
    nothing_installed(&w.path("root"));
}

#[test]
fn refuses_a_large_package_without_chunk_hashes() {
    let w = World::new();
    let mut entry = pack_big(&w);
    entry.chunks = None;
    let path = w.path("repo/v1/26/1/1/24B56/latest/arm64e/packages.d/big.json");
    fs::write(&path, serde_json::to_vec(&entry).unwrap()).unwrap();
    w.publish(1, None, "signer");
    fails_with(&w.kl(&["add", "big"]), "lists no chunk hashes");
    nothing_installed(&w.path("root"));
}

#[test]
fn refuses_a_signature_from_another_key() {
    let w = World::new();
    w.keygen("stranger");
    w.pack_both();
    w.publish(1, None, "stranger");
    fails_with(&w.kl(&["add", "foo"]), "not from a trusted key");
    nothing_installed(&w.path("root"));
}

#[test]
fn refuses_expired_metadata() {
    let w = World::new();
    w.pack_both();
    w.publish(1, Some("2000-01-01T00:00:00Z"), "signer");
    fails_with(&w.kl(&["add", "foo"]), "expired");
    nothing_installed(&w.path("root"));
}

#[test]
fn refuses_to_go_back_to_older_metadata() {
    let w = World::new();
    w.pack_both();
    w.publish(5, None, "signer");
    ok(&w.kl(&["update"]));
    w.publish(4, None, "signer");
    fails_with(&w.kl(&["add", "foo"]), "older than version 5");
    nothing_installed(&w.path("root"));
}

#[test]
fn refuses_to_overwrite_a_file_no_package_owns() {
    let w = World::new();
    w.pack_both();
    w.publish(1, None, "signer");
    let root = w.path("root");
    fs::create_dir_all(root.join("usr/lib")).unwrap();
    fs::write(root.join("usr/lib/libfoo.dylib"), b"the system's own").unwrap();
    fails_with(&w.kl(&["add", "foo"]), "exists and no package owns it");
    assert_eq!(
        fs::read(root.join("usr/lib/libfoo.dylib")).unwrap(),
        b"the system's own"
    );
    assert!(!root.join("usr/share").exists());
}

fn evil_blob(manifest: &Manifest, extra: &[(&str, &[u8])]) -> Vec<u8> {
    let mut encoder = zstd::stream::write::Encoder::new(Vec::new(), 3).unwrap();
    {
        let mut tar = tar::Builder::new(&mut encoder);
        let json = serde_json::to_vec(manifest).unwrap();
        let mut h = tar::Header::new_gnu();
        h.set_size(json.len() as u64);
        h.set_mode(0o644);
        tar.append_data(&mut h, ".kl/manifest.json", json.as_slice())
            .unwrap();
        for (path, data) in extra {
            let mut h = tar::Header::new_gnu();
            h.set_size(data.len() as u64);
            h.set_mode(0o644);
            let name = h.as_old_mut().name.as_mut();
            name[..path.len()].copy_from_slice(path.as_bytes());
            h.set_cksum();
            tar.append(&h, *data).unwrap();
        }
        tar.finish().unwrap();
    }
    encoder.finish().unwrap()
}

fn publish_evil(w: &World, manifest: &Manifest, blob: &[u8]) {
    let blob_ref = kido_trust::BlobRef::of(blob);
    fs::create_dir_all(w.path("repo/v1/blobs/sha256")).unwrap();
    fs::write(w.path("repo/v1/blobs/sha256").join(&blob_ref.sha256), blob).unwrap();
    let files: Vec<String> = manifest
        .files
        .iter()
        .filter(|f| f.kind != Kind::Dir)
        .map(|f| f.path.clone())
        .collect();
    let package = kl::index::Package {
        name: manifest.name.clone(),
        version: manifest.version.clone(),
        summary: String::new(),
        license: "MIT".into(),
        blob: blob_ref,
        installed_size: 1 << 20,
        chunks: None,
        provides: vec![],
        depends: vec![],
        files,
    };
    let target_dir = w.path("repo/v1/26/1/1/24B56/latest/arm64e");
    fs::create_dir_all(&target_dir).unwrap();
    let target = RepoTarget::new(
        "v1",
        OsVersion::parse(DEFAULT_OS).unwrap(),
        DEFAULT_BUILD,
        "latest",
    )
    .unwrap();
    let idx = kl::index::Index {
        version: 1,
        target: target.clone(),
        arch: DEFAULT_ARCH.into(),
        packages: vec![package],
    };
    let idx_bytes = serde_json::to_vec_pretty(&idx).unwrap();
    let idx_ref = kido_trust::BlobRef::of(&idx_bytes);
    fs::write(
        target_dir.join(format!("index.{}.json", idx_ref.sha256)),
        &idx_bytes,
    )
    .unwrap();
    let ts = kido_trust::Timestamp {
        version: 1,
        expires: "2099-01-01T00:00:00Z".into(),
        target,
        indexes: [(DEFAULT_ARCH.to_owned(), idx_ref)].into(),
    };
    let stamp = w.path("repo/v1/26/1/1/24B56/latest/timestamp.json");
    fs::write(&stamp, serde_json::to_vec_pretty(&ts).unwrap()).unwrap();
    let key = w.path("signer");
    w.repo(&["sign", "--key", key.to_str().unwrap()]);
}

fn evil_manifest(files: Vec<FileEntry>) -> Manifest {
    Manifest {
        name: "evil".into(),
        version: "1".into(),
        arch: DEFAULT_ARCH.into(),
        license: "MIT".into(),
        summary: String::new(),
        provides: vec![],
        depends: vec![],
        files,
    }
}

#[test]
fn refuses_a_package_that_writes_outside_its_manifest() {
    let w = World::new();
    let data: &[u8] = b"pwned";
    let manifest = evil_manifest(vec![FileEntry {
        path: "usr/lib/ok.dylib".into(),
        kind: Kind::File,
        mode: 0o644,
        size: Some(data.len() as u64),
        sha256: Some(kido_trust::sha256_hex(data)),
        target: None,
    }]);
    let blob = evil_blob(&manifest, &[("usr/lib/../../etc/evil", data)]);
    publish_evil(&w, &manifest, &blob);
    fails_with(&w.kl(&["add", "evil"]), "is not in the manifest");
    nothing_installed(&w.path("root"));
    assert!(!w.path("etc").exists());
}

#[test]
fn refuses_a_manifest_with_a_path_outside_the_managed_directories() {
    let w = World::new();
    let data: &[u8] = b"pwned";
    let manifest = evil_manifest(vec![FileEntry {
        path: "etc/sudoers".into(),
        kind: Kind::File,
        mode: 0o644,
        size: Some(data.len() as u64),
        sha256: Some(kido_trust::sha256_hex(data)),
        target: None,
    }]);
    let blob = evil_blob(&manifest, &[("etc/sudoers", data)]);
    publish_evil(&w, &manifest, &blob);
    fails_with(
        &w.kl(&["add", "evil"]),
        "outside the directories kl manages",
    );
    nothing_installed(&w.path("root"));
}

#[test]
fn refuses_a_symlink_that_escapes() {
    let w = World::new();
    let manifest = evil_manifest(vec![FileEntry {
        path: "usr/lib/escape".into(),
        kind: Kind::Symlink,
        mode: 0o777,
        size: None,
        sha256: None,
        target: Some("../../etc".into()),
    }]);
    let blob = evil_blob(&manifest, &[]);
    publish_evil(&w, &manifest, &blob);
    fails_with(
        &w.kl(&["add", "evil"]),
        "points outside the directories kl manages",
    );
    nothing_installed(&w.path("root"));
}

#[test]
fn finishes_an_interrupted_transaction() {
    let w = World::new();
    let root = w.path("root");
    let db = Db::open(&root).unwrap();
    let staged = db.staging().join("foo/usr/lib");
    fs::create_dir_all(&staged).unwrap();
    fs::write(staged.join("libfoo.dylib"), b"foo").unwrap();
    let manifest = Manifest {
        name: "foo".into(),
        version: "1.0".into(),
        arch: DEFAULT_ARCH.into(),
        license: "MIT".into(),
        summary: String::new(),
        provides: vec![],
        depends: vec![],
        files: vec![FileEntry {
            path: "usr/lib/libfoo.dylib".into(),
            kind: Kind::File,
            mode: 0o644,
            size: Some(3),
            sha256: Some(kido_trust::sha256_hex(b"foo")),
            target: None,
        }],
    };
    let journal = Journal {
        ops: vec![Op::Rename {
            from: "var/db/kl/staging/foo/usr/lib/libfoo.dylib".into(),
            to: "usr/lib/libfoo.dylib".into(),
        }],
        put: vec![Installed {
            manifest,
            blob: kido_trust::BlobRef::of(b"blob"),
        }],
        world: ["foo".to_owned()].into(),
        ..Journal::default()
    };
    let journal_path = db.journal();
    let staging_path = db.staging();
    fs::write(&journal_path, serde_json::to_vec(&journal).unwrap()).unwrap();
    // Simulate process exit after crash so the OS releases the exclusive DB lock.
    drop(db);
    let out = w.kl(&["list"]);
    ok(&out);
    assert!(String::from_utf8_lossy(&out.stderr).contains("finished an interrupted transaction"));
    assert_eq!(String::from_utf8(out.stdout).unwrap(), "foo 1.0\n");
    assert_eq!(fs::read(root.join("usr/lib/libfoo.dylib")).unwrap(), b"foo");
    assert!(!journal_path.exists());
    assert!(!staging_path.exists());
}

#[test]
fn refuses_cross_build_timestamp_replay_attack() {
    let w = World::new();
    let repo = w.path("repo");
    // Publish legitimately signed metadata for beta build 24B52 AND release build 24B56.
    w.pack_both_in(&repo, "26.1.1", "24B52", "latest");
    w.publish_in(&repo, ("26.1.1", "24B52", "latest"), 1, None, "signer");
    w.pack_both_in(&repo, "26.1.1", "24B56", "latest");
    w.publish_in(&repo, ("26.1.1", "24B56", "latest"), 1, None, "signer");

    // Attacker replaces 24B56's timestamp.json and .sig with 24B52's legitimately signed files.
    let src_dir = repo.join("v1/26/1/1/24B52/latest");
    let dst_dir = repo.join("v1/26/1/1/24B56/latest");
    fs::copy(
        src_dir.join("timestamp.json"),
        dst_dir.join("timestamp.json"),
    )
    .unwrap();
    fs::copy(
        src_dir.join("timestamp.json.sig"),
        dst_dir.join("timestamp.json.sig"),
    )
    .unwrap();

    // Client targeting 24B56 MUST cryptographically reject 24B52's signed timestamp!
    fails_with(
        &w.kl(&["add", "foo"]),
        "signed metadata is for target v1/26/1/1/24B52/latest, not v1/26/1/1/24B56/latest",
    );
    nothing_installed(&w.path("root"));
}

#[test]
fn rejects_path_traversal_and_malformed_target_cli_flags() {
    let w = World::new();
    let repo_url = format!("file://{}", w.path("repo").display());
    let keys = w.path("signer.pub");
    let keys = keys.to_str().unwrap();

    for extra in [
        vec![
            "--kl-version",
            "../v1",
            "--os-version",
            "26.1.1",
            "--os-build",
            "24B56",
        ],
        vec![
            "--kl-version",
            "v0",
            "--os-version",
            "26.1.1",
            "--os-build",
            "24B56",
        ],
        vec!["--os-version", "26.1.1", "--os-build", "../24B56"],
        vec!["--os-version", "26.1.1", "--os-build", ".hidden"],
        vec![
            "--os-version",
            "26.1.1",
            "--os-build",
            "24B56",
            "--channel",
            "../latest",
        ],
        vec![
            "--os-version",
            "26.1.1",
            "--os-build",
            "24B56",
            "--channel",
            "beta",
        ],
        vec!["--os-version", "26", "--os-build", "24B56"],
    ] {
        let mut args = vec!["--repo", &repo_url, "--keys", keys, "--arch", DEFAULT_ARCH];
        args.extend(extra);
        args.push("update");
        let out = w.kl_raw(&args);
        assert!(
            !out.status.success(),
            "expected failure for {args:?}, got stdout: {}",
            String::from_utf8_lossy(&out.stdout)
        );
    }
}

#[test]
fn deduplicates_shared_blobs_across_multiple_os_builds() {
    let w = World::new();
    let repo = w.path("repo");
    // Publish identical packages for 26.1.1/24B52, 26.1.1/24B56, and 27.0/26B118.
    w.pack_both_in(&repo, "26.1.1", "24B52", "latest");
    w.publish_in(&repo, ("26.1.1", "24B52", "latest"), 1, None, "signer");
    w.pack_both_in(&repo, "26.1.1", "24B56", "latest");
    w.publish_in(&repo, ("26.1.1", "24B56", "latest"), 1, None, "signer");
    w.pack_both_in(&repo, "27.0", "26B118", "latest");
    w.publish_in(&repo, ("27.0", "26B118", "latest"), 1, None, "signer");

    // Only 2 blobs total (foo + bar) should exist in v1/blobs/sha256 across all 3 builds!
    let blob_count = fs::read_dir(repo.join("v1/blobs/sha256")).unwrap().count();
    assert_eq!(
        blob_count, 2,
        "expected exactly 2 deduplicated blobs across 3 OS builds"
    );
    assert!(
        repo.join("v1/26/1/1/24B52/latest/timestamp.json.sig")
            .is_file()
    );
    assert!(
        repo.join("v1/26/1/1/24B56/latest/timestamp.json.sig")
            .is_file()
    );
    assert!(
        repo.join("v1/27/0/0/26B118/latest/timestamp.json.sig")
            .is_file()
    );

    // Install from 27.0/26B118 (padded to 27/0/0) into a fresh root!
    let repo_url = format!("file://{}", repo.display());
    let keys = w.path("signer.pub");
    ok(&w.kl_raw(&[
        "--repo",
        &repo_url,
        "--os-version",
        "27.0",
        "--os-build",
        "26B118",
        "--arch",
        DEFAULT_ARCH,
        "--keys",
        keys.to_str().unwrap(),
        "add",
        "bar",
    ]));
    assert_eq!(
        fs::read(w.path("root/usr/lib/libbar.dylib")).unwrap(),
        b"bar"
    );
}

#[test]
fn incrementally_indexes_new_package_without_repacking_existing_ones() {
    let w = World::new();
    let repo = w.path("repo");
    w.pack_both_in(&repo, DEFAULT_OS, DEFAULT_BUILD, "latest");
    w.publish_in(
        &repo,
        (DEFAULT_OS, DEFAULT_BUILD, "latest"),
        1,
        None,
        "signer",
    );

    // Simulate a synced repo where packages.d/ is absent and only published index + blobs remain.
    let pkg_d = repo.join("v1/26/1/1/24B56/latest/arm64e/packages.d");
    fs::remove_dir_all(&pkg_d).unwrap();
    let blobs_before: Vec<_> = fs::read_dir(repo.join("v1/blobs/sha256"))
        .unwrap()
        .map(|e| e.unwrap().file_name())
        .collect();
    assert_eq!(blobs_before.len(), 2);

    // Pack ONLY a new package C ("py") and run kl-repo index + timestamp + sign.
    let from = w.path("tree");
    World::repo_in(
        &repo,
        DEFAULT_OS,
        DEFAULT_BUILD,
        "latest",
        &[
            "pack",
            "--arch",
            DEFAULT_ARCH,
            "--from",
            from.to_str().unwrap(),
            "--name",
            "py",
            "--version",
            "3.13",
            "--license",
            "PSF-2.0",
            "--summary",
            "python3 runtime",
            "usr/bin/python3",
        ],
    );
    w.publish_in(
        &repo,
        (DEFAULT_OS, DEFAULT_BUILD, "latest"),
        2,
        None,
        "signer",
    );

    // Existing A ("foo") and B ("bar") plus new C ("py") are all in the index and installable!
    let search_out = w.kl(&["search", ""]);
    ok(&search_out);
    let listing = String::from_utf8(search_out.stdout).unwrap();
    assert!(listing.contains("foo 1.0"), "{listing}");
    assert!(listing.contains("bar 2.0"), "{listing}");
    assert!(listing.contains("py 3.13"), "{listing}");

    ok(&w.kl(&["add", "bar", "py"]));
    assert_eq!(
        fs::read(w.path("root/usr/bin/python3")).unwrap(),
        b"#!/bin/sh\necho py\n"
    );
    assert_eq!(
        fs::read(w.path("root/usr/lib/libfoo.dylib")).unwrap(),
        b"not really a dylib"
    );
}

#[test]
fn auto_detects_system_version_plist_and_respects_kl_conf() {
    let w = World::new();
    let repo = w.path("repo");
    // Publish for 26.5 / 25F79 / latest (padded to 26/5/0).
    w.pack_both_in(&repo, "26.5", "25F79", "latest");
    w.publish_in(&repo, ("26.5", "25F79", "latest"), 1, None, "signer");

    let root = w.path("root");
    fs::create_dir_all(root.join("System/Library/CoreServices")).unwrap();
    fs::write(
        root.join("System/Library/CoreServices/SystemVersion.plist"),
        r#"<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>ProductBuildVersion</key>
	<string>25F79</string>
	<key>ProductName</key>
	<string>KidoDarwin</string>
	<key>ProductVersion</key>
	<string>26.5</string>
</dict>
</plist>"#,
    )
    .unwrap();
    fs::create_dir_all(root.join("etc")).unwrap();
    fs::write(
        root.join("etc/kl.conf"),
        format!(
            "repo = file://{}\nkl_version = v1\nchannel = latest\narch = arm64e\n",
            repo.display()
        ),
    )
    .unwrap();

    // Run kl with NO --repo, --os-version, --os-build, --channel, or --arch flags!
    let keys = w.path("signer.pub");
    ok(&w.kl_raw(&["--keys", keys.to_str().unwrap(), "add", "foo"]));
    assert_eq!(
        fs::read(root.join("usr/lib/libfoo.dylib")).unwrap(),
        b"not really a dylib"
    );

    // Rollback state is keyed per target (v1/26/5/0/25F79/latest).
    w.pack_both_in(&repo, "26.5", "25F79", "nightly");
    w.publish_in(&repo, ("26.5", "25F79", "nightly"), 2, None, "signer");
    ok(&w.kl_raw(&[
        "--keys",
        keys.to_str().unwrap(),
        "--channel",
        "nightly",
        "update",
    ]));
    w.publish_in(&repo, ("26.5", "25F79", "nightly"), 1, None, "signer");
    fails_with(
        &w.kl_raw(&[
            "--keys",
            keys.to_str().unwrap(),
            "--channel",
            "nightly",
            "update",
        ]),
        "older than version 2",
    );
    // Latest channel still works at version 1 (independent rollback counter per target/channel).
    ok(&w.kl_raw(&["--keys", keys.to_str().unwrap(), "update"]));
}

#[test]
fn which_answers_for_both_dylibs_and_non_dylib_binaries() {
    let w = World::new();
    let repo = w.path("repo");
    let from = w.tree();
    let from = from.to_str().unwrap();
    World::repo_in(
        &repo,
        DEFAULT_OS,
        DEFAULT_BUILD,
        "latest",
        &[
            "pack",
            "--arch",
            DEFAULT_ARCH,
            "--from",
            from,
            "--name",
            "foo",
            "--version",
            "1.0",
            "--license",
            "MIT",
            "usr/lib/libfoo.dylib",
        ],
    );
    World::repo_in(
        &repo,
        DEFAULT_OS,
        DEFAULT_BUILD,
        "latest",
        &[
            "pack",
            "--arch",
            DEFAULT_ARCH,
            "--from",
            from,
            "--name",
            "python3",
            "--version",
            "3.13",
            "--license",
            "PSF-2.0",
            "usr/bin/python3",
        ],
    );
    w.publish(1, None, "signer");

    // Before installation: both dylib AND non-dylib binary are found via the signed index!
    let py_before = w.kl(&["which", "/usr/bin/python3"]);
    ok(&py_before);
    assert_eq!(
        String::from_utf8(py_before.stdout).unwrap(),
        "/usr/bin/python3: not installed; it comes with python3 (kl add python3)\n"
    );
    let lib_before = w.kl(&["which", "/usr/lib/libfoo.dylib"]);
    ok(&lib_before);
    assert_eq!(
        String::from_utf8(lib_before.stdout).unwrap(),
        "/usr/lib/libfoo.dylib: not installed; it comes with foo (kl add foo)\n"
    );

    // After installation: reported directly from installed database!
    ok(&w.kl(&["add", "python3"]));
    let py_after = w.kl(&["which", "/usr/bin/python3"]);
    ok(&py_after);
    assert_eq!(
        String::from_utf8(py_after.stdout).unwrap(),
        "/usr/bin/python3: python3\n"
    );
}

#[test]
fn end_to_end_http_server_verifies_exact_v1_wire_urls() {
    let w = World::new();
    let repo = w.path("repo");
    w.pack_both_in(&repo, "27.0", "26B118", "latest");
    w.publish_in(&repo, ("27.0", "26B118", "latest"), 1, None, "signer");

    let server = HttpRepoServer::start(repo);
    let keys = w.path("signer.pub");
    let keys = keys.to_str().unwrap();

    ok(&w.kl_raw(&[
        "--repo",
        &server.url,
        "--os-version",
        "27.0",
        "--os-build",
        "26B118",
        "--arch",
        DEFAULT_ARCH,
        "--keys",
        keys,
        "add",
        "bar",
    ]));
    ok(&w.kl_raw(&["verify"]));

    let paths = server.recorded_paths();
    assert!(
        paths
            .iter()
            .any(|p| p == "/v1/27/0/0/26B118/latest/timestamp.json"),
        "expected padded v1/27/0/0 timestamp URL in {paths:?}"
    );
    assert!(
        paths
            .iter()
            .any(|p| p == "/v1/27/0/0/26B118/latest/timestamp.json.sig"),
        "expected timestamp.json.sig URL in {paths:?}"
    );
    assert!(
        paths.iter().any(|p| {
            p.starts_with("/v1/27/0/0/26B118/latest/arm64e/index.")
                && Path::new(p)
                    .extension()
                    .is_some_and(|e| e.eq_ignore_ascii_case("json"))
        }),
        "expected hash-addressed index URL in {paths:?}"
    );
    assert!(
        paths.iter().any(|p| p.starts_with("/v1/blobs/sha256/")),
        "expected shared v1/blobs/sha256 URL in {paths:?}"
    );

    ok(&w.kl_raw(&["--arch", DEFAULT_ARCH, "rm", "bar"]));
    assert!(
        Db::open(&w.path("root"))
            .unwrap()
            .installed()
            .unwrap()
            .is_empty()
    );
}

#[test]
fn zero_trust_mirror_installs_and_falls_back_on_down_redirect_or_corrupt_chunk() {
    let w = World::new();
    let primary_dir = w.path("repo");
    let big_entry = pack_big(&w);
    w.pack_both();
    w.publish(2, None, "signer");

    // 1. Primary anchors freshness via tiny timestamp.json; mirror serves index and blobs.
    let good_mirror_dir = w.path("mirror-good");
    copy_dir_all(&primary_dir, &good_mirror_dir);
    let good_mirror = HttpRepoServer::start(good_mirror_dir);
    let primary_srv = HttpRepoServer::start(primary_dir.clone());
    let keys = w.path("signer.pub");
    let keys = keys.to_str().unwrap();

    ok(&w.kl_raw(&[
        "--repo",
        &primary_srv.url,
        "--mirror",
        &good_mirror.url,
        "--os-version",
        DEFAULT_OS,
        "--os-build",
        DEFAULT_BUILD,
        "--arch",
        DEFAULT_ARCH,
        "--keys",
        keys,
        "add",
        "foo",
    ]));
    assert!(
        good_mirror
            .recorded_paths()
            .iter()
            .any(|p| p.starts_with("/v1/blobs/sha256/")),
        "mirror should have served the package blob"
    );
    assert!(
        primary_srv
            .recorded_paths()
            .iter()
            .all(|p| p.ends_with("/timestamp.json") || p.ends_with("/timestamp.json.sig")),
        "primary should only serve timestamp metadata when mirror is healthy: {:?}",
        primary_srv.recorded_paths()
    );
    ok(&w.kl_raw(&["verify"]));

    // 2. Fallback when mirror 1 returns 503, mirror 2 returns 302 redirect, and mirror 3 corrupts chunk 1!
    let down_mirror = HttpRepoServer::start_error();
    let redirect_mirror = HttpRepoServer::start_redirect();
    let bad_chunk_mirror_dir = w.path("mirror-bad-chunk");
    copy_dir_all(&primary_dir, &bad_chunk_mirror_dir);
    let bad_blob = bad_chunk_mirror_dir
        .join("v1/blobs/sha256")
        .join(&big_entry.blob.sha256);
    let mut bytes = fs::read(&bad_blob).unwrap();
    bytes[usize::try_from(kido_trust::CHUNK_SIZE).unwrap() + 17] ^= 0xff;
    fs::write(&bad_blob, bytes).unwrap();
    let bad_chunk_mirror = HttpRepoServer::start(bad_chunk_mirror_dir);

    let out = w.kl_raw(&[
        "--repo",
        &primary_srv.url,
        "--mirror",
        &down_mirror.url,
        "--mirror",
        &redirect_mirror.url,
        "--mirror",
        &bad_chunk_mirror.url,
        "--os-version",
        DEFAULT_OS,
        "--os-build",
        DEFAULT_BUILD,
        "--arch",
        DEFAULT_ARCH,
        "--keys",
        keys,
        "add",
        "big",
    ]);
    ok(&out);
    let stderr = String::from_utf8_lossy(&out.stderr);
    assert!(stderr.contains("trying next source"), "{stderr}");
    assert!(
        stderr.contains("302"),
        "expected 302 refusal warning: {stderr}"
    );
    assert!(
        stderr.contains("chunk 1 of the blob does not match"),
        "{stderr}"
    );
    assert!(
        primary_srv
            .recorded_paths()
            .iter()
            .any(|p| p.starts_with("/v1/blobs/sha256/"))
    );
    ok(&w.kl_raw(&["verify"]));
}

#[test]
fn zero_trust_mirror_falls_back_on_stale_or_wrong_build_metadata() {
    let w = World::new();
    let primary_dir = w.path("repo");
    w.pack_both();
    w.publish(2, None, "signer");
    let primary_srv = HttpRepoServer::start(primary_dir);
    let keys = w.path("signer.pub");
    let keys = keys.to_str().unwrap();

    // Advance client's seen timestamp version to 2 first.
    ok(&w.kl_raw(&[
        "--repo",
        &primary_srv.url,
        "--os-version",
        DEFAULT_OS,
        "--os-build",
        DEFAULT_BUILD,
        "--arch",
        DEFAULT_ARCH,
        "--keys",
        keys,
        "update",
    ]));

    // Simulate primary outage (503) so mirrors' timestamps are evaluated directly.
    let down_primary = HttpRepoServer::start_error();

    let stale_mirror_dir = w.path("mirror-stale");
    w.pack_both_in(&stale_mirror_dir, DEFAULT_OS, DEFAULT_BUILD, "latest");
    w.publish_in(
        &stale_mirror_dir,
        (DEFAULT_OS, DEFAULT_BUILD, "latest"),
        1,
        None,
        "signer",
    );
    let stale_mirror = HttpRepoServer::start(stale_mirror_dir);

    let wrong_build_dir = w.path("mirror-wrong-build");
    w.pack_both_in(&wrong_build_dir, "26.1.1", "24B52", "latest");
    w.publish_in(
        &wrong_build_dir,
        ("26.1.1", "24B52", "latest"),
        9,
        None,
        "signer",
    );
    let evil_dst = wrong_build_dir.join("v1/26/1/1/24B56/latest");
    fs::create_dir_all(&evil_dst).unwrap();
    fs::copy(
        wrong_build_dir.join("v1/26/1/1/24B52/latest/timestamp.json"),
        evil_dst.join("timestamp.json"),
    )
    .unwrap();
    fs::copy(
        wrong_build_dir.join("v1/26/1/1/24B52/latest/timestamp.json.sig"),
        evil_dst.join("timestamp.json.sig"),
    )
    .unwrap();
    let wrong_build_mirror = HttpRepoServer::start(wrong_build_dir);

    // Fresh mirror at version 3 (higher than seen=2 and stale=1).
    let fresh_mirror_dir = w.path("mirror-fresh");
    w.pack_both_in(&fresh_mirror_dir, DEFAULT_OS, DEFAULT_BUILD, "latest");
    w.publish_in(
        &fresh_mirror_dir,
        (DEFAULT_OS, DEFAULT_BUILD, "latest"),
        3,
        None,
        "signer",
    );
    let fresh_mirror = HttpRepoServer::start(fresh_mirror_dir);

    let root = w.path("root");
    fs::create_dir_all(root.join("etc")).unwrap();
    fs::write(
        root.join("etc/kl.conf"),
        format!(
            "repo = {}\nmirror = {}\nmirror = {}\nmirror = {}\nos_version = {DEFAULT_OS}\nos_build = {DEFAULT_BUILD}\narch = {DEFAULT_ARCH}\n",
            down_primary.url, stale_mirror.url, wrong_build_mirror.url, fresh_mirror.url
        ),
    )
    .unwrap();

    let out = w.kl_raw(&["--keys", keys, "add", "bar"]);
    ok(&out);
    let stderr = String::from_utf8_lossy(&out.stderr);
    assert!(stderr.contains("older than version 2"), "{stderr}");
    assert!(
        stderr.contains("24B52/latest, not v1/26/1/1/24B56/latest"),
        "{stderr}"
    );
    assert_eq!(fs::read(root.join("usr/lib/libbar.dylib")).unwrap(), b"bar");
    ok(&w.kl_raw(&["verify"]));

    // Also test permission tampering detection by `kl verify` (e.g. chmod 0777).
    fs::set_permissions(
        root.join("usr/lib/libbar.dylib"),
        fs::Permissions::from_mode(0o777),
    )
    .unwrap();
    let verify_bad_mode = w.kl_raw(&["verify"]);
    assert!(!verify_bad_mode.status.success());
    assert!(
        String::from_utf8_lossy(&verify_bad_mode.stdout).contains("usr/lib/libbar.dylib: changed")
    );
}
