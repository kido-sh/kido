use kl::db::{Db, Installed};
use kl::install::{Journal, Op};
use kl::manifest::{FileEntry, Kind, Manifest};
use std::fs;
use std::os::unix::fs::symlink;
use std::path::{Path, PathBuf};
use std::process::{Command, Output};

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

    fn repo(&self, args: &[&str]) -> Output {
        let out = Command::new(env!("CARGO_BIN_EXE_kl-repo"))
            .arg("--repo")
            .arg(self.path("repo"))
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

    fn kl_with(&self, keys: &str, args: &[&str]) -> Output {
        Command::new(env!("CARGO_BIN_EXE_kl"))
            .arg("--root")
            .arg(self.path("root"))
            .arg("--repo")
            .arg(format!("file://{}", self.path("repo").display()))
            .arg("--arch")
            .arg("arm64e")
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
        fs::create_dir_all(from.join("usr/share/foo/doc")).unwrap();
        fs::write(from.join("usr/lib/libfoo.dylib"), b"not really a dylib").unwrap();
        symlink("libfoo.dylib", from.join("usr/lib/libfoo.1.dylib")).unwrap();
        fs::write(from.join("usr/share/foo/data.txt"), b"foo data\n").unwrap();
        fs::write(from.join("usr/share/foo/doc/README"), b"read me\n").unwrap();
        fs::write(from.join("usr/lib/libbar.dylib"), b"bar").unwrap();
        from
    }

    fn publish(&self, version: u64, expires: Option<&str>, key: &str) {
        let v = version.to_string();
        self.repo(&["index", "--arch", "arm64e", "--version", &v]);
        let mut ts = vec!["timestamp", "--version", &v];
        if let Some(e) = expires {
            ts.extend(["--expires", e]);
        }
        self.repo(&ts);
        let key = self.path(key);
        self.repo(&["sign", "--key", key.to_str().unwrap()]);
    }

    fn pack_both(&self) {
        let from = self.tree();
        let from = from.to_str().unwrap();
        self.repo(&[
            "pack",
            "--arch",
            "arm64e",
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
        ]);
        self.repo(&[
            "pack",
            "--arch",
            "arm64e",
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
        ]);
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
    let blobs = w.path("repo/blobs/sha256");
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
    let entry = w.path("repo/latest/arm64e/packages.d/foo.json");
    let mut package: kl::index::Package =
        serde_json::from_slice(&fs::read(&entry).unwrap()).unwrap();
    let blob = w.path("repo/blobs/sha256").join(&package.blob.sha256);
    let plain = zstd::decode_all(fs::read(&blob).unwrap().as_slice()).unwrap();
    let other = zstd::encode_all(plain.as_slice(), 1).unwrap();
    package.blob.size = other.len() as u64;
    fs::write(&blob, other).unwrap();
    fs::write(&entry, serde_json::to_vec(&package).unwrap()).unwrap();
    w.publish(2, None, "signer");
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
        "arm64e",
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
    serde_json::from_slice(&fs::read(w.path("repo/latest/arm64e/packages.d/big.json")).unwrap())
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
    let blob = w.path("repo/blobs/sha256").join(&entry.blob.sha256);
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
    let path = w.path("repo/latest/arm64e/packages.d/big.json");
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
    fs::create_dir_all(w.path("repo/blobs/sha256")).unwrap();
    fs::write(w.path("repo/blobs/sha256").join(&blob_ref.sha256), blob).unwrap();
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
    };
    let dir = w.path("repo/latest/arm64e/packages.d");
    fs::create_dir_all(&dir).unwrap();
    fs::write(dir.join("evil.json"), serde_json::to_vec(&package).unwrap()).unwrap();
    w.publish(1, None, "signer");
}

fn evil_manifest(files: Vec<FileEntry>) -> Manifest {
    Manifest {
        name: "evil".into(),
        version: "1".into(),
        arch: "arm64e".into(),
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
        arch: "arm64e".into(),
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
    fs::write(db.journal(), serde_json::to_vec(&journal).unwrap()).unwrap();
    let out = w.kl(&["list"]);
    ok(&out);
    assert!(String::from_utf8_lossy(&out.stderr).contains("finished an interrupted transaction"));
    assert_eq!(String::from_utf8(out.stdout).unwrap(), "foo 1.0\n");
    assert_eq!(fs::read(root.join("usr/lib/libfoo.dylib")).unwrap(), b"foo");
    assert!(!db.journal().exists());
    assert!(!db.staging().exists());
}
