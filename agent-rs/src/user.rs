use std::fs::File;
use std::io::{self, BufRead, BufReader};
use std::path::Path;

pub const PASSWD_FILE: &str = "/etc/passwd";
pub const GROUP_FILE: &str = "/etc/group";
const NGROUPS_MAX: usize = 16;
const MAX_DB_LINE: usize = 64 << 10;
const MAX_DB_BYTES: usize = 8 << 20;

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Account {
    pub uid: u32,
    pub gid: u32,
    pub groups: Vec<u32>,
    pub home: String,
}

pub fn lookup_user(spec: &str) -> Result<Account, String> {
    lookup_user_from(spec, Path::new(PASSWD_FILE), Path::new(GROUP_FILE))
}

pub fn lookup_user_from(spec: &str, passwd_path: &Path, group_path: &Path) -> Result<Account, String> {
    let (user_part, group_part) = match spec.split_once(':') {
        Some((u, g)) => (u, Some(g)),
        None => (spec, None),
    };
    if user_part.is_empty() || matches!(group_part, Some("")) {
        return Err(format!("invalid user {spec:?}"));
    }

    let uid;
    let mut gid = 0u32;
    let mut home = "/".to_string();
    let mut name = String::new();

    if let Some(id) = parse_id(user_part) {
        uid = id;
        let entry = find_passwd(passwd_path, |e| parse_db_id(&e[2]) == Some(id))
            .map_err(|e| e.to_string())?;
        if let Some(e) = entry {
            let Some(g) = parse_db_id(&e[3]) else {
                return Err(format!(
                    "bad gid for {} in {}",
                    e[0],
                    passwd_path.display()
                ));
            };
            name = e[0].clone();
            gid = g;
            home = e[5].clone();
        }
    } else {
        if !valid_name(user_part) {
            return Err(format!("invalid user name {user_part:?}"));
        }
        let entry = find_passwd(passwd_path, |e| e[0] == user_part).map_err(|e| e.to_string())?;
        let Some(e) = entry else {
            return Err(format!(
                "unable to find user {user_part}: no matching entries in passwd file"
            ));
        };
        let Some(v) = parse_db_id(&e[2]) else {
            return Err(format!(
                "bad uid for {user_part} in {}",
                passwd_path.display()
            ));
        };
        let Some(g) = parse_db_id(&e[3]) else {
            return Err(format!(
                "bad gid for {user_part} in {}",
                passwd_path.display()
            ));
        };
        name = e[0].clone();
        uid = v;
        gid = g;
        home = e[5].clone();
    }

    let group_rows = match read_db(group_path, 4) {
        Ok(rows) => rows,
        Err(e) if e.kind() == io::ErrorKind::NotFound => Vec::new(),
        Err(e) => return Err(e.to_string()),
    };

    if let Some(gp) = group_part {
        if let Some(id) = parse_id(gp) {
            gid = id;
        } else {
            if !valid_name(gp) {
                return Err(format!("invalid group name {gp:?}"));
            }
            let mut found = false;
            for g in &group_rows {
                if g[0] == gp
                    && let Some(v) = parse_db_id(&g[2])
                {
                    gid = v;
                    found = true;
                    break;
                }
            }
            if !found {
                return Err(format!(
                    "unable to find group {gp}: no matching entries in group file"
                ));
            }
        }
    }

    let mut groups = vec![gid];
    if !name.is_empty() {
        for g in &group_rows {
            if groups.len() >= NGROUPS_MAX {
                break;
            }
            let Some(g_id) = parse_db_id(&g[2]) else {
                continue;
            };
            if groups.contains(&g_id) {
                continue;
            }
            if g[3].split(',').any(|m| m.trim() == name) {
                groups.push(g_id);
            }
        }
    }
    if home.is_empty() {
        home = "/".to_string();
    }
    Ok(Account {
        uid,
        gid,
        groups,
        home,
    })
}

fn valid_name(s: &str) -> bool {
    let b = s.as_bytes();
    if b.is_empty() || b.len() > 255 {
        return false;
    }
    if !(b[0].is_ascii_alphabetic() || b[0] == b'_') {
        return false;
    }
    b[1..]
        .iter()
        .all(|&c| c.is_ascii_alphanumeric() || matches!(c, b'_' | b'.' | b'-'))
}

fn parse_id(s: &str) -> Option<u32> {
    if s.is_empty() || !s.bytes().all(|b| b.is_ascii_digit()) {
        return None;
    }
    s.parse::<u32>().ok()
}

fn parse_db_id(s: &str) -> Option<u32> {
    let v = s.trim().parse::<i64>().ok()?;
    if !(-(1i64 << 31)..=(1i64 << 32) - 1).contains(&v) {
        return None;
    }
    Some(v as u32)
}

fn find_passwd<F>(path: &Path, mut pred: F) -> io::Result<Option<Vec<String>>>
where
    F: FnMut(&[String]) -> bool,
{
    let entries = match read_db(path, 7) {
        Ok(e) => e,
        Err(e) if e.kind() == io::ErrorKind::NotFound => return Ok(None),
        Err(e) => return Err(e),
    };
    Ok(entries.into_iter().find(|e| pred(e)))
}

fn read_bounded_line<R: BufRead>(
    r: &mut R,
    line: &mut Vec<u8>,
    total_bytes: &mut usize,
) -> io::Result<bool> {
    line.clear();
    loop {
        let buf = match r.fill_buf() {
            Ok(b) => b,
            Err(ref e) if e.kind() == io::ErrorKind::Interrupted => continue,
            Err(e) => return Err(e),
        };
        if buf.is_empty() {
            return Ok(!line.is_empty());
        }
        if let Some(pos) = buf.iter().position(|&b| b == b'\n') {
            let take = pos + 1;
            *total_bytes = total_bytes.saturating_add(take);
            if *total_bytes > MAX_DB_BYTES || line.len() + pos > MAX_DB_LINE {
                r.consume(take);
                return Err(io::Error::new(
                    io::ErrorKind::InvalidData,
                    "database entry exceeds size limit",
                ));
            }
            line.extend_from_slice(&buf[..pos]);
            r.consume(take);
            return Ok(true);
        }
        let n = buf.len();
        *total_bytes = total_bytes.saturating_add(n);
        if *total_bytes > MAX_DB_BYTES || line.len() + n > MAX_DB_LINE {
            r.consume(n);
            return Err(io::Error::new(
                io::ErrorKind::InvalidData,
                "database entry exceeds size limit",
            ));
        }
        line.extend_from_slice(buf);
        r.consume(n);
    }
}

fn read_db(path: &Path, min_fields: usize) -> io::Result<Vec<Vec<String>>> {
    let f = File::open(path)?;
    let mut reader = BufReader::new(f);
    let mut out = Vec::new();
    let mut raw = Vec::new();
    let mut total_bytes = 0usize;
    while read_bounded_line(&mut reader, &mut raw, &mut total_bytes)? {
        let decoded = String::from_utf8_lossy(&raw);
        let trimmed = decoded.trim();
        if trimmed.is_empty() || trimmed.starts_with('#') {
            continue;
        }
        let fields: Vec<String> = trimmed.split(':').map(str::to_string).collect();
        if fields.len() >= min_fields {
            out.push(fields);
        }
    }
    Ok(out)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::fs;

    #[test]
    fn lookup_user_parity() {
        let dir = std::env::temp_dir().join(format!("kido-agent-user-test-{}", std::process::id()));
        let _ = fs::remove_dir_all(&dir);
        fs::create_dir_all(&dir).unwrap();
        let passwd = dir.join("passwd");
        let group = dir.join("group");
        fs::write(
            &passwd,
            "# comment\nroot:*:0:0:System Administrator:/var/root:/bin/sh\nnobody:*:-2:-2:Unprivileged User:/var/empty:/usr/bin/false\nalice:*:501:20::/Users/alice:/bin/zsh\n",
        )
        .unwrap();
        fs::write(
            &group,
            "wheel:*:0:root\nstaff:*:20:root\nadmin:*:80:alice,root\nnobody:*:-2:\n",
        )
        .unwrap();

        let cases = [
            ("root", 0, 0, vec![0, 20, 80], "/var/root"),
            ("alice", 501, 20, vec![20, 80], "/Users/alice"),
            ("nobody", 4294967294, 4294967294, vec![4294967294], "/var/empty"),
            ("501", 501, 20, vec![20, 80], "/Users/alice"),
            ("1234", 1234, 0, vec![0], "/"),
            ("1234:56", 1234, 56, vec![56], "/"),
            ("alice:wheel", 501, 0, vec![0, 80], "/Users/alice"),
        ];
        for (spec, uid, gid, groups, home) in cases {
            let a = lookup_user_from(spec, &passwd, &group).unwrap();
            assert_eq!(
                a,
                Account {
                    uid,
                    gid,
                    groups,
                    home: home.into()
                },
                "spec {spec}"
            );
        }
        for bad in ["", ":", "bob", "alice:", "alice:nogroup", "../x", "-1"] {
            assert!(lookup_user_from(bad, &passwd, &group).is_err(), "{bad}");
        }
        let _ = fs::remove_dir_all(&dir);
    }

    #[test]
    fn non_utf8_passwd_does_not_degrade_numeric_uid_to_gid_zero() {
        let dir =
            std::env::temp_dir().join(format!("kido-agent-user-utf8-{}", std::process::id()));
        let _ = fs::remove_dir_all(&dir);
        fs::create_dir_all(&dir).unwrap();
        let passwd = dir.join("passwd");
        let group = dir.join("group");

        let mut raw = Vec::new();
        raw.extend_from_slice(b"latin1:*:500:20:Jos\xe9:/Users/latin1:/bin/sh\n");
        raw.extend_from_slice(b"alice:*:501:20:Alice:/Users/alice:/bin/zsh\n");
        raw.extend_from_slice(b"badgid:*:502:notanumber:Bad:/Users/bad:/bin/sh\n");
        fs::write(&passwd, &raw).unwrap();

        let mut grp = String::new();
        grp.push_str("dup1:*:80:alice\ndup2:*:80:alice\n");
        for i in 100..130 {
            grp.push_str(&format!("g{i}:*:{i}:alice\n"));
        }
        fs::write(&group, grp).unwrap();

        let a = lookup_user_from("501", &passwd, &group).unwrap();
        assert_eq!(a.uid, 501);
        assert_eq!(a.gid, 20);
        assert_eq!(a.home, "/Users/alice");
        assert_eq!(a.groups.len(), NGROUPS_MAX);
        assert_eq!(a.groups.iter().filter(|&&g| g == 80).count(), 1);

        let l = lookup_user_from("latin1", &passwd, &group).unwrap();
        assert_eq!(l.uid, 500);
        assert_eq!(l.gid, 20);

        assert!(lookup_user_from("badgid", &passwd, &group).is_err());
        assert!(lookup_user_from("502", &passwd, &group).is_err());
        let _ = fs::remove_dir_all(&dir);
    }
}
