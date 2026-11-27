use std::collections::HashMap;
use std::io;
use std::os::unix::io::{AsRawFd, FromRawFd, OwnedFd, RawFd};
use std::sync::Arc;

pub const EV_READ: u32 = 1;
pub const EV_WRITE: u32 = 2;

#[derive(Debug, Clone, Copy)]
pub struct PollEvent {
    pub token: u64,
    pub writable: bool,
    pub hup_or_err: bool,
}

struct WakeupInner {
    read_fd: OwnedFd,
    #[cfg(not(target_os = "linux"))]
    write_fd: OwnedFd,
}

pub struct Wakeup {
    inner: Arc<WakeupInner>,
}

impl Wakeup {
    pub fn new() -> io::Result<Self> {
        #[cfg(target_os = "linux")]
        {
            let fd = unsafe { libc::eventfd(0, libc::EFD_NONBLOCK | libc::EFD_CLOEXEC) };
            if fd < 0 {
                return Err(io::Error::last_os_error());
            }
            Ok(Self {
                inner: Arc::new(WakeupInner {
                    read_fd: unsafe { OwnedFd::from_raw_fd(fd) },
                }),
            })
        }
        #[cfg(not(target_os = "linux"))]
        {
            let mut fds = [0 as libc::c_int; 2];
            if unsafe { libc::pipe(fds.as_mut_ptr()) } != 0 {
                return Err(io::Error::last_os_error());
            }
            for fd in fds {
                let flags = unsafe { libc::fcntl(fd, libc::F_GETFL) };
                unsafe {
                    libc::fcntl(fd, libc::F_SETFL, flags | libc::O_NONBLOCK);
                    libc::fcntl(fd, libc::F_SETFD, libc::FD_CLOEXEC);
                }
            }
            Ok(Self {
                inner: Arc::new(WakeupInner {
                    read_fd: unsafe { OwnedFd::from_raw_fd(fds[0]) },
                    write_fd: unsafe { OwnedFd::from_raw_fd(fds[1]) },
                }),
            })
        }
    }

    pub fn raw_fd(&self) -> RawFd {
        self.inner.read_fd.as_raw_fd()
    }

    pub fn waker(&self) -> Waker {
        Waker {
            inner: Arc::clone(&self.inner),
        }
    }

    pub fn drain(&self) {
        let fd = self.inner.read_fd.as_raw_fd();
        #[cfg(target_os = "linux")]
        {
            let mut buf = 0u64;
            loop {
                let n = unsafe {
                    libc::read(
                        fd,
                        (&mut buf as *mut u64).cast::<libc::c_void>(),
                        std::mem::size_of::<u64>(),
                    )
                };
                if n < 0 && io::Error::last_os_error().raw_os_error() == Some(libc::EINTR) {
                    continue;
                }
                break;
            }
        }
        #[cfg(not(target_os = "linux"))]
        {
            let mut buf = [0u8; 64];
            loop {
                let n = unsafe {
                    libc::read(
                        fd,
                        buf.as_mut_ptr().cast::<libc::c_void>(),
                        buf.len(),
                    )
                };
                if n < 0 && io::Error::last_os_error().raw_os_error() == Some(libc::EINTR) {
                    continue;
                }
                if n <= 0 {
                    break;
                }
            }
        }
    }
}

#[derive(Clone)]
pub struct Waker {
    inner: Arc<WakeupInner>,
}

impl Waker {
    pub fn wake(&self) {
        #[cfg(target_os = "linux")]
        {
            let fd = self.inner.read_fd.as_raw_fd();
            let val: u64 = 1;
            loop {
                let n = unsafe {
                    libc::write(
                        fd,
                        (&val as *const u64).cast::<libc::c_void>(),
                        std::mem::size_of::<u64>(),
                    )
                };
                if n < 0 && io::Error::last_os_error().raw_os_error() == Some(libc::EINTR) {
                    continue;
                }
                break;
            }
        }
        #[cfg(not(target_os = "linux"))]
        {
            let fd = self.inner.write_fd.as_raw_fd();
            let b: u8 = 1;
            loop {
                let n = unsafe { libc::write(fd, (&b as *const u8).cast::<libc::c_void>(), 1) };
                if n < 0 && io::Error::last_os_error().raw_os_error() == Some(libc::EINTR) {
                    continue;
                }
                break;
            }
        }
    }
}

pub struct Poller {
    #[cfg(target_os = "linux")]
    epfd: OwnedFd,
    entries: HashMap<RawFd, (u64, u32)>,
}

impl Poller {
    pub fn new() -> io::Result<Self> {
        #[cfg(target_os = "linux")]
        {
            let epfd = unsafe { libc::epoll_create1(libc::EPOLL_CLOEXEC) };
            if epfd < 0 {
                return Err(io::Error::last_os_error());
            }
            Ok(Self {
                epfd: unsafe { OwnedFd::from_raw_fd(epfd) },
                entries: HashMap::new(),
            })
        }
        #[cfg(not(target_os = "linux"))]
        {
            Ok(Self {
                entries: HashMap::new(),
            })
        }
    }

    pub fn register(&mut self, fd: RawFd, token: u64, interest: u32) -> io::Result<()> {
        #[cfg(target_os = "linux")]
        if interest != 0 {
            let mut ev = libc::epoll_event {
                events: Self::to_epoll_events(interest),
                u64: token,
            };
            let rc = unsafe {
                libc::epoll_ctl(self.epfd.as_raw_fd(), libc::EPOLL_CTL_ADD, fd, &mut ev)
            };
            if rc != 0 {
                return Err(io::Error::last_os_error());
            }
        }
        self.entries.insert(fd, (token, interest));
        Ok(())
    }

    pub fn modify(&mut self, fd: RawFd, token: u64, interest: u32) -> io::Result<()> {
        let Some(&(_, cur)) = self.entries.get(&fd) else {
            return Ok(());
        };
        if cur == interest {
            return Ok(());
        }
        #[cfg(target_os = "linux")]
        {
            if cur == 0 && interest != 0 {
                let mut ev = libc::epoll_event {
                    events: Self::to_epoll_events(interest),
                    u64: token,
                };
                let rc = unsafe {
                    libc::epoll_ctl(self.epfd.as_raw_fd(), libc::EPOLL_CTL_ADD, fd, &mut ev)
                };
                if rc != 0 {
                    return Err(io::Error::last_os_error());
                }
            } else if cur != 0 && interest == 0 {
                let rc = unsafe {
                    libc::epoll_ctl(
                        self.epfd.as_raw_fd(),
                        libc::EPOLL_CTL_DEL,
                        fd,
                        std::ptr::null_mut(),
                    )
                };
                if rc != 0 {
                    let err = io::Error::last_os_error();
                    if err.raw_os_error() != Some(libc::ENOENT) {
                        return Err(err);
                    }
                }
            } else {
                let mut ev = libc::epoll_event {
                    events: Self::to_epoll_events(interest),
                    u64: token,
                };
                let rc = unsafe {
                    libc::epoll_ctl(self.epfd.as_raw_fd(), libc::EPOLL_CTL_MOD, fd, &mut ev)
                };
                if rc != 0 {
                    return Err(io::Error::last_os_error());
                }
            }
        }
        self.entries.insert(fd, (token, interest));
        Ok(())
    }

    pub fn deregister(&mut self, fd: RawFd) {
        if let Some((_, cur)) = self.entries.remove(&fd) {
            #[cfg(target_os = "linux")]
            if cur != 0 {
                unsafe {
                    libc::epoll_ctl(
                        self.epfd.as_raw_fd(),
                        libc::EPOLL_CTL_DEL,
                        fd,
                        std::ptr::null_mut(),
                    );
                }
            }
            #[cfg(not(target_os = "linux"))]
            let _ = cur;
        }
    }

    pub fn wait(&self, out: &mut Vec<PollEvent>, timeout_ms: i32) -> io::Result<()> {
        out.clear();
        #[cfg(target_os = "linux")]
        {
            let mut raw = [libc::epoll_event { events: 0, u64: 0 }; 64];
            let n = unsafe {
                libc::epoll_wait(
                    self.epfd.as_raw_fd(),
                    raw.as_mut_ptr(),
                    raw.len() as i32,
                    timeout_ms,
                )
            };
            if n < 0 {
                let err = io::Error::last_os_error();
                if err.raw_os_error() == Some(libc::EINTR) {
                    return Ok(());
                }
                return Err(err);
            }
            for ev in &raw[..n as usize] {
                let bits = ev.events;
                out.push(PollEvent {
                    token: ev.u64,
                    writable: (bits & (libc::EPOLLOUT as u32)) != 0,
                    hup_or_err: (bits & ((libc::EPOLLERR | libc::EPOLLHUP) as u32)) != 0,
                });
            }
            Ok(())
        }
        #[cfg(not(target_os = "linux"))]
        {
            let mut pfds = Vec::with_capacity(self.entries.len());
            let mut tokens = Vec::with_capacity(self.entries.len());
            for (&fd, &(token, interest)) in &self.entries {
                if interest == 0 {
                    continue;
                }
                let mut ev: libc::c_short = 0;
                if (interest & EV_READ) != 0 {
                    ev |= libc::POLLIN;
                }
                if (interest & EV_WRITE) != 0 {
                    ev |= libc::POLLOUT;
                }
                pfds.push(libc::pollfd {
                    fd,
                    events: ev,
                    revents: 0,
                });
                tokens.push(token);
            }
            let n = unsafe {
                libc::poll(pfds.as_mut_ptr(), pfds.len() as libc::nfds_t, timeout_ms)
            };
            if n < 0 {
                let err = io::Error::last_os_error();
                if err.raw_os_error() == Some(libc::EINTR) {
                    return Ok(());
                }
                return Err(err);
            }
            for (pfd, token) in pfds.iter().zip(tokens.iter()) {
                if pfd.revents == 0 {
                    continue;
                }
                out.push(PollEvent {
                    token: *token,
                    writable: (pfd.revents & libc::POLLOUT) != 0,
                    hup_or_err: (pfd.revents & (libc::POLLERR | libc::POLLHUP | libc::POLLNVAL))
                        != 0,
                });
            }
            Ok(())
        }
    }

    #[cfg(target_os = "linux")]
    fn to_epoll_events(interest: u32) -> u32 {
        let mut ev = (libc::EPOLLERR | libc::EPOLLHUP) as u32;
        if (interest & EV_READ) != 0 {
            ev |= (libc::EPOLLIN | libc::EPOLLRDHUP) as u32;
        }
        if (interest & EV_WRITE) != 0 {
            ev |= libc::EPOLLOUT as u32;
        }
        ev
    }
}
