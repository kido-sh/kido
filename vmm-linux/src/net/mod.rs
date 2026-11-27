mod poll;

use std::collections::{HashMap, VecDeque};
use std::io::{self, Read, Write};
use std::net::{Ipv4Addr, Shutdown, SocketAddrV4, TcpListener, TcpStream, UdpSocket};
use std::os::unix::io::{AsRawFd, FromRawFd, RawFd};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Condvar, Mutex};
use std::thread::{self, JoinHandle};
use std::time::{Duration, Instant as StdInstant};

use smoltcp::iface::{Config as IfaceConfig, Interface, SocketHandle, SocketSet};
use smoltcp::phy::{ChecksumCapabilities, Device, DeviceCapabilities, Medium, RxToken, TxToken};
use smoltcp::socket::tcp::{Socket as TcpSocket, SocketBuffer as TcpSocketBuffer, State};
use smoltcp::time::Instant as SmolInstant;
use smoltcp::wire::{
    ArpOperation, ArpPacket, ArpRepr, EthernetAddress, EthernetFrame, EthernetProtocol,
    EthernetRepr, HardwareAddress, IpAddress, IpCidr, IpEndpoint, IpProtocol, Ipv4Address,
    Ipv4Packet, Ipv4Repr, TcpControl, TcpPacket, TcpRepr, UdpPacket, UdpRepr,
};

use crate::boot::read_urandom_seed;
use crate::net::poll::{PollEvent, Poller, Wakeup, Waker, EV_READ, EV_WRITE};
use crate::virtio::net::{NetChannel, VIRTIO_NET_MAX_FRAME};

pub const DEFAULT_GUEST_MAC: [u8; 6] = [0x02, 0xdc, 0x72, 0x00, 0x00, 0x01];
pub const ROUTER_MAC: [u8; 6] = [0x52, 0x55, 0x0a, 0x00, 0x02, 0x02];

pub const GUEST_IP: Ipv4Addr = Ipv4Addr::new(10, 0, 2, 15);
pub const GATEWAY_IP: Ipv4Addr = Ipv4Addr::new(10, 0, 2, 2);
pub const DNS_IP: Ipv4Addr = Ipv4Addr::new(10, 0, 2, 3);

const MAX_INBOUND_TCP_SESSIONS: usize = 64;
const MAX_OUTBOUND_TCP_SESSIONS: usize = 192;
const MAX_UDP_SESSIONS: usize = 256;
const MAX_UDP_RECV_PER_TICK: usize = 64;
const MAX_TCP_ACCEPT_PER_TICK: usize = 32;
const TCP_SOCKET_BUF_SIZE: usize = 64 * 1024;
const UDP_IDLE_TIMEOUT: Duration = Duration::from_secs(60);
const TCP_CONNECT_TIMEOUT: Duration = Duration::from_secs(15);
const TCP_LISTEN_TIMEOUT: Duration = Duration::from_secs(10);
const TCP_CLOSE_DRAIN_TIMEOUT: Duration = Duration::from_secs(2);
const MAX_FRAME_LEN: usize = 1514;

const TOKEN_KIND_SHIFT: u32 = 60;
const TOKEN_ID_MASK: u64 = (1u64 << TOKEN_KIND_SHIFT) - 1;
const KIND_WAKE: u64 = 1 << TOKEN_KIND_SHIFT;
const KIND_TCP_FWD: u64 = 2 << TOKEN_KIND_SHIFT;
const KIND_UDP_FWD: u64 = 3 << TOKEN_KIND_SHIFT;
const KIND_TCP_CONN: u64 = 4 << TOKEN_KIND_SHIFT;
const KIND_UDP_OUT: u64 = 5 << TOKEN_KIND_SHIFT;
const TOKEN_KIND_MASK: u64 = 0xfu64 << TOKEN_KIND_SHIFT;

const TOKEN_WAKE: u64 = KIND_WAKE;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ForwardProto {
    Tcp,
    Udp,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct ForwardSpec {
    pub proto: ForwardProto,
    pub host_ip: Ipv4Addr,
    pub host_port: u16,
    pub guest_ip: Ipv4Addr,
    pub guest_port: u16,
}

pub fn parse_mac(s: &str) -> Result<[u8; 6], String> {
    let parts: Vec<&str> = s.split(':').collect();
    if parts.len() != 6 {
        return Err(format!(
            "invalid MAC '{s}': expected 6 colon-separated hex octets"
        ));
    }
    let mut mac = [0u8; 6];
    for (i, p) in parts.iter().enumerate() {
        if p.len() != 2 {
            return Err(format!("invalid MAC octet '{p}' in '{s}'"));
        }
        mac[i] = u8::from_str_radix(p, 16).map_err(|_| format!("invalid MAC '{s}'"))?;
    }
    if mac == [0; 6] {
        return Err(format!("invalid MAC '{s}': all-zero address not allowed"));
    }
    if (mac[0] & 1) != 0 {
        return Err(format!("invalid MAC '{s}': multicast address not allowed"));
    }
    Ok(mac)
}

pub fn parse_forward(spec: &str) -> Result<ForwardSpec, String> {
    let parts: Vec<&str> = spec.split(':').collect();
    let (proto_s, host_ip_s, host_port_s, guest_ip_s, guest_port_s) = match parts.as_slice() {
        [p, hip, hp, gp] => (*p, *hip, *hp, None, *gp),
        [p, hip, hp, gip, gp] => (*p, *hip, *hp, Some(*gip), *gp),
        _ => {
            return Err(format!(
                "invalid --forward '{spec}': expected tcp|udp:HOST_IP:HOST_PORT:[GUEST_IP:]GUEST_PORT"
            ))
        }
    };
    let proto = match proto_s {
        "tcp" => ForwardProto::Tcp,
        "udp" => ForwardProto::Udp,
        _ => {
            return Err(format!(
                "invalid --forward protocol '{proto_s}' (expected tcp or udp)"
            ))
        }
    };
    let host_ip: Ipv4Addr = if host_ip_s.is_empty() {
        Ipv4Addr::LOCALHOST
    } else {
        host_ip_s
            .parse()
            .map_err(|_| format!("invalid --forward host IP '{host_ip_s}'"))?
    };
    let host_port: u16 = host_port_s
        .parse()
        .map_err(|_| format!("invalid --forward host port '{host_port_s}'"))?;
    let guest_ip: Ipv4Addr = match guest_ip_s {
        Some(g) => g
            .parse()
            .map_err(|_| format!("invalid --forward guest IP '{g}'"))?,
        None => GUEST_IP,
    };
    let guest_port: u16 = guest_port_s
        .parse()
        .map_err(|_| format!("invalid --forward guest port '{guest_port_s}'"))?;
    if guest_port == 0 {
        return Err(format!("invalid --forward guest port 0 in '{spec}'"));
    }
    Ok(ForwardSpec {
        proto,
        host_ip,
        host_port,
        guest_ip,
        guest_port,
    })
}

pub fn is_blocked_outbound_ip(ip: Ipv4Addr) -> bool {
    let o = ip.octets();
    ip.is_loopback()
        || ip.is_unspecified()
        || o[0] == 0
        || o[0] >= 240
        || ip.is_link_local()
        || ip.is_multicast()
        || ip.is_broadcast()
        || (o[0] == 10 && o[1] == 0 && o[2] == 2)
}

fn parse_resolv_conf_str(text: &str) -> Vec<Ipv4Addr> {
    let mut out = Vec::new();
    for line in text.lines() {
        let mut parts = line.split_whitespace();
        if parts.next() == Some("nameserver") {
            if let Some(tok) = parts.next() {
                if let Ok(ip) = tok.parse::<Ipv4Addr>() {
                    if !ip.is_unspecified() && !out.contains(&ip) {
                        out.push(ip);
                    }
                }
            }
        }
    }
    if out.is_empty() {
        out.push(Ipv4Addr::new(1, 1, 1, 1));
        out.push(Ipv4Addr::new(8, 8, 8, 8));
    }
    out
}

fn read_resolv_conf_ipv4() -> Vec<Ipv4Addr> {
    match std::fs::read_to_string("/etc/resolv.conf") {
        Ok(text) => parse_resolv_conf_str(&text),
        Err(_) => parse_resolv_conf_str(""),
    }
}

fn smol_ipv4(ip: Ipv4Addr) -> Ipv4Address {
    let o = ip.octets();
    Ipv4Address::new(o[0], o[1], o[2], o[3])
}

fn std_ipv4(ip: Ipv4Address) -> Ipv4Addr {
    let o = ip.octets();
    Ipv4Addr::new(o[0], o[1], o[2], o[3])
}

struct NetPauseState {
    pause_requested: bool,
    paused: bool,
    stopped: bool,
}

pub struct NetPauseControl {
    state: Mutex<NetPauseState>,
    cv: Condvar,
    waker: Waker,
}

impl NetPauseControl {
    fn new(waker: Waker) -> Arc<Self> {
        Arc::new(Self {
            state: Mutex::new(NetPauseState {
                pause_requested: false,
                paused: false,
                stopped: false,
            }),
            cv: Condvar::new(),
            waker,
        })
    }

    pub fn pause(&self) {
        let mut st = self.state.lock().unwrap();
        if st.stopped {
            return;
        }
        st.pause_requested = true;
        drop(st);
        self.waker.wake();
        let mut st = self.state.lock().unwrap();
        while !st.paused && !st.stopped {
            st = self.cv.wait(st).unwrap();
        }
    }

    pub fn resume(&self) {
        let mut st = self.state.lock().unwrap();
        st.pause_requested = false;
        self.cv.notify_all();
        drop(st);
        self.waker.wake();
    }

    pub fn wake_shutdown(&self) {
        let mut st = self.state.lock().unwrap();
        st.pause_requested = false;
        self.cv.notify_all();
        drop(st);
        self.waker.wake();
    }

    fn mark_stopped(&self) {
        let mut st = self.state.lock().unwrap();
        st.stopped = true;
        st.paused = false;
        self.cv.notify_all();
    }
}

struct VirtioPhy {
    ingress: VecDeque<Vec<u8>>,
    egress: VecDeque<Vec<u8>>,
}

impl VirtioPhy {
    fn new() -> Self {
        Self {
            ingress: VecDeque::with_capacity(64),
            egress: VecDeque::with_capacity(64),
        }
    }
}

struct PhyRxToken {
    frame: Vec<u8>,
}

impl RxToken for PhyRxToken {
    fn consume<R, F>(self, f: F) -> R
    where
        F: FnOnce(&[u8]) -> R,
    {
        f(&self.frame)
    }
}

struct PhyTxToken<'a> {
    egress: &'a mut VecDeque<Vec<u8>>,
}

impl<'a> TxToken for PhyTxToken<'a> {
    fn consume<R, F>(self, len: usize, f: F) -> R
    where
        F: FnOnce(&mut [u8]) -> R,
    {
        let mut buf = vec![0u8; len];
        let res = f(&mut buf);
        self.egress.push_back(buf);
        res
    }
}

impl Device for VirtioPhy {
    type RxToken<'a> = PhyRxToken;
    type TxToken<'a> = PhyTxToken<'a>;

    fn receive(&mut self, _timestamp: SmolInstant) -> Option<(Self::RxToken<'_>, Self::TxToken<'_>)> {
        let frame = self.ingress.pop_front()?;
        Some((
            PhyRxToken { frame },
            PhyTxToken {
                egress: &mut self.egress,
            },
        ))
    }

    fn transmit(&mut self, _timestamp: SmolInstant) -> Option<Self::TxToken<'_>> {
        Some(PhyTxToken {
            egress: &mut self.egress,
        })
    }

    fn capabilities(&self) -> DeviceCapabilities {
        let mut caps = DeviceCapabilities::default();
        caps.medium = Medium::Ethernet;
        caps.max_transmission_unit = MAX_FRAME_LEN;
        caps.checksum = ChecksumCapabilities::default();
        caps
    }
}

struct TcpForwardListener {
    spec: ForwardSpec,
    listener: TcpListener,
}

struct UdpForwardListener {
    spec: ForwardSpec,
    socket: UdpSocket,
}

struct TcpSession {
    token: u64,
    handle: SocketHandle,
    is_inbound: bool,
    host: Option<TcpStream>,
    host_fd: RawFd,
    host_connecting: bool,
    connecting_since: Option<StdInstant>,
    listen_since: Option<StdInstant>,
    pending_syn: Option<(Vec<u8>, i32)>,
    syn_sent_has_data: bool,
    host_eof: bool,
    guest_eof: bool,
    pending_to_host: VecDeque<u8>,
    outbound_tuple: Option<(Ipv4Addr, u16, Ipv4Addr, u16)>,
    time_wait_since: Option<StdInstant>,
    closing_drain_since: Option<StdInstant>,
}

struct UdpInboundSession {
    fwd_idx: usize,
    host_peer: SocketAddrV4,
    guest_ip: Ipv4Addr,
    guest_port: u16,
    last_active: StdInstant,
}

struct UdpOutboundSession {
    guest_src_ip: Ipv4Addr,
    guest_src_port: u16,
    virtual_dst_ip: Ipv4Addr,
    virtual_dst_port: u16,
    expected_peer: SocketAddrV4,
    socket: UdpSocket,
    last_active: StdInstant,
}

pub struct NetEngine {
    channel: Arc<NetChannel>,
    guest_mac: [u8; 6],
    restrict: bool,
    dns_upstreams: Vec<Ipv4Addr>,
    start_instant: StdInstant,
    paused_total: Duration,
    connect_timeout: Duration,
    phy: VirtioPhy,
    iface: Interface,
    sockets: SocketSet<'static>,
    poller: Poller,
    wakeup: Wakeup,
    tcp_forwards: Vec<TcpForwardListener>,
    udp_forwards: Vec<UdpForwardListener>,
    tcp_sessions: HashMap<u64, TcpSession>,
    next_tcp_seq: u64,
    next_ephemeral_port: u16,
    udp_inbound_by_ephem: HashMap<u16, UdpInboundSession>,
    udp_inbound_by_peer: HashMap<(usize, SocketAddrV4), u16>,
    udp_outbound_by_token: HashMap<u64, UdpOutboundSession>,
    udp_outbound_by_key: HashMap<(Ipv4Addr, u16, Ipv4Addr, u16), u64>,
    next_udp_seq: u64,
    ip_id: u16,
}

impl Drop for NetEngine {
    fn drop(&mut self) {
        self.channel.clear_wake_net();
    }
}

impl NetEngine {
    pub fn bind(
        channel: Arc<NetChannel>,
        guest_mac: [u8; 6],
        forwards: &[ForwardSpec],
        restrict: bool,
    ) -> Result<Self, String> {
        let wakeup = Wakeup::new().map_err(|e| format!("create net wakeup fd: {e}"))?;
        let mut poller = Poller::new().map_err(|e| format!("create net poller: {e}"))?;
        poller
            .register(wakeup.raw_fd(), TOKEN_WAKE, EV_READ)
            .map_err(|e| format!("register wakeup fd: {e}"))?;

        let mut tcp_forwards = Vec::new();
        let mut udp_forwards = Vec::new();

        for &spec in forwards {
            let bind_addr = SocketAddrV4::new(spec.host_ip, spec.host_port);
            match spec.proto {
                ForwardProto::Tcp => {
                    let listener = TcpListener::bind(bind_addr).map_err(|e| {
                        format!("bind tcp forward {bind_addr} -> {}: {e}", spec.guest_port)
                    })?;
                    listener
                        .set_nonblocking(true)
                        .map_err(|e| format!("set tcp listener nonblocking: {e}"))?;
                    let idx = tcp_forwards.len();
                    let actual_port = listener
                        .local_addr()
                        .map(|a| a.port())
                        .unwrap_or(spec.host_port);
                    poller
                        .register(
                            listener.as_raw_fd(),
                            KIND_TCP_FWD | (idx as u64),
                            EV_READ,
                        )
                        .map_err(|e| format!("register tcp listener: {e}"))?;
                    let mut actual_spec = spec;
                    actual_spec.host_port = actual_port;
                    tcp_forwards.push(TcpForwardListener {
                        spec: actual_spec,
                        listener,
                    });
                }
                ForwardProto::Udp => {
                    let socket = UdpSocket::bind(bind_addr).map_err(|e| {
                        format!("bind udp forward {bind_addr} -> {}: {e}", spec.guest_port)
                    })?;
                    socket
                        .set_nonblocking(true)
                        .map_err(|e| format!("set udp listener nonblocking: {e}"))?;
                    let idx = udp_forwards.len();
                    let actual_port = socket
                        .local_addr()
                        .map(|a| a.port())
                        .unwrap_or(spec.host_port);
                    poller
                        .register(
                            socket.as_raw_fd(),
                            KIND_UDP_FWD | (idx as u64),
                            EV_READ,
                        )
                        .map_err(|e| format!("register udp listener: {e}"))?;
                    let mut actual_spec = spec;
                    actual_spec.host_port = actual_port;
                    udp_forwards.push(UdpForwardListener {
                        spec: actual_spec,
                        socket,
                    });
                }
            }
        }

        let seed_bytes = read_urandom_seed()?;
        let random_seed = u64::from_le_bytes(seed_bytes[0..8].try_into().unwrap());
        let start_instant = StdInstant::now();
        let mut phy = VirtioPhy::new();
        let mut cfg = IfaceConfig::new(HardwareAddress::Ethernet(EthernetAddress(ROUTER_MAC)));
        cfg.random_seed = random_seed;
        let mut iface = Interface::new(cfg, &mut phy, SmolInstant::from_millis(0));
        iface.update_ip_addrs(|addrs| {
            let _ = addrs.push(IpCidr::new(IpAddress::Ipv4(smol_ipv4(GATEWAY_IP)), 24));
            let _ = addrs.push(IpCidr::new(IpAddress::Ipv4(smol_ipv4(DNS_IP)), 24));
        });
        let _ = iface
            .routes_mut()
            .add_default_ipv4_route(smol_ipv4(GATEWAY_IP));
        iface.set_any_ip(true);

        let waker = wakeup.waker();
        channel.set_wake_net(Arc::new(move || waker.wake()));

        let mut eng = Self {
            channel,
            guest_mac,
            restrict,
            dns_upstreams: read_resolv_conf_ipv4(),
            start_instant,
            paused_total: Duration::ZERO,
            connect_timeout: TCP_CONNECT_TIMEOUT,
            phy,
            iface,
            sockets: SocketSet::new(vec![]),
            poller,
            wakeup,
            tcp_forwards,
            udp_forwards,
            tcp_sessions: HashMap::new(),
            next_tcp_seq: 1,
            next_ephemeral_port: 40000,
            udp_inbound_by_ephem: HashMap::new(),
            udp_inbound_by_peer: HashMap::new(),
            udp_outbound_by_token: HashMap::new(),
            udp_outbound_by_key: HashMap::new(),
            next_udp_seq: 1,
            ip_id: 1,
        };
        eng.seed_guest_neighbor(GUEST_IP);
        Ok(eng)
    }

    fn alloc_tcp_token(&mut self) -> u64 {
        loop {
            let seq = self.next_tcp_seq & TOKEN_ID_MASK;
            self.next_tcp_seq = (seq + 1) & TOKEN_ID_MASK;
            let tok = KIND_TCP_CONN | seq;
            if !self.tcp_sessions.contains_key(&tok) {
                return tok;
            }
        }
    }

    fn alloc_udp_token(&mut self) -> u64 {
        loop {
            let seq = self.next_udp_seq & TOKEN_ID_MASK;
            self.next_udp_seq = (seq + 1) & TOKEN_ID_MASK;
            let tok = KIND_UDP_OUT | seq;
            if !self.udp_outbound_by_token.contains_key(&tok) {
                return tok;
            }
        }
    }

    fn inbound_tcp_count(&self) -> usize {
        self.tcp_sessions.values().filter(|s| s.is_inbound).count()
    }

    fn outbound_tcp_count(&self) -> usize {
        self.tcp_sessions.values().filter(|s| !s.is_inbound).count()
    }

    fn seed_guest_neighbor(&mut self, guest_ip: Ipv4Addr) {
        let arp_repr = ArpRepr::EthernetIpv4 {
            operation: ArpOperation::Reply,
            source_hardware_addr: EthernetAddress(self.guest_mac),
            source_protocol_addr: smol_ipv4(guest_ip),
            target_hardware_addr: EthernetAddress(ROUTER_MAC),
            target_protocol_addr: smol_ipv4(GATEWAY_IP),
        };
        let mut frame = vec![0u8; 14 + arp_repr.buffer_len()];
        let mut eth = EthernetFrame::new_unchecked(&mut frame);
        EthernetRepr {
            src_addr: EthernetAddress(self.guest_mac),
            dst_addr: EthernetAddress(ROUTER_MAC),
            ethertype: EthernetProtocol::Arp,
        }
        .emit(&mut eth);
        arp_repr.emit(&mut ArpPacket::new_unchecked(eth.payload_mut()));
        self.phy.ingress.push_back(frame);
        let now = self.now_smol();
        self.iface.poll(now, &mut self.phy, &mut self.sockets);
    }

    #[cfg(test)]
    pub fn bound_tcp_port(&self, idx: usize) -> Option<u16> {
        self.tcp_forwards.get(idx).map(|f| f.spec.host_port)
    }

    #[cfg(test)]
    pub fn bound_udp_port(&self, idx: usize) -> Option<u16> {
        self.udp_forwards.get(idx).map(|f| f.spec.host_port)
    }

    pub fn spawn(
        mut self,
        shutdown: Arc<AtomicBool>,
    ) -> Result<(Arc<NetPauseControl>, JoinHandle<()>), String> {
        let pause_ctrl = NetPauseControl::new(self.wakeup.waker());
        let pause_thread = Arc::clone(&pause_ctrl);
        let handle = thread::Builder::new()
            .name("virtio-net".to_string())
            .spawn(move || {
                self.run_loop(&shutdown, &pause_thread);
                pause_thread.mark_stopped();
            })
            .map_err(|e| format!("spawn virtio-net thread: {e}"))?;
        Ok((pause_ctrl, handle))
    }

    fn now_smol(&self) -> SmolInstant {
        let active = self
            .start_instant
            .elapsed()
            .saturating_sub(self.paused_total);
        SmolInstant::from_millis(active.as_millis() as i64)
    }

    fn alloc_ephemeral_port(&mut self) -> u16 {
        for _ in 0..20000 {
            let p = self.next_ephemeral_port;
            self.next_ephemeral_port = if p >= 59999 { 40000 } else { p + 1 };
            let in_tcp = self.tcp_sessions.values().any(|s| {
                self.sockets
                    .get::<TcpSocket>(s.handle)
                    .local_endpoint()
                    .is_some_and(|ep| ep.port == p)
            });
            if !in_tcp && !self.udp_inbound_by_ephem.contains_key(&p) {
                return p;
            }
        }
        40000
    }

    fn run_loop(&mut self, shutdown: &AtomicBool, pause_ctrl: &NetPauseControl) {
        let mut events = Vec::with_capacity(64);
        let mut tx_batch = Vec::with_capacity(64);
        let mut last_gc = StdInstant::now();

        while !shutdown.load(Ordering::Acquire) {
            {
                let mut st = pause_ctrl.state.lock().unwrap();
                if st.pause_requested {
                    let pause_start = StdInstant::now();
                    st.paused = true;
                    pause_ctrl.cv.notify_all();
                    while st.pause_requested && !shutdown.load(Ordering::Acquire) {
                        st = pause_ctrl.cv.wait(st).unwrap();
                    }
                    st.paused = false;
                    self.paused_total += pause_start.elapsed();
                    if shutdown.load(Ordering::Acquire) {
                        break;
                    }
                }
            }
            self.step(&mut events, &mut tx_batch, &mut last_gc, true);
        }
    }

    pub fn step(
        &mut self,
        events: &mut Vec<PollEvent>,
        tx_batch: &mut Vec<Vec<u8>>,
        last_gc: &mut StdInstant,
        block: bool,
    ) {
        tx_batch.clear();
        self.channel.drain_tx_to_net(tx_batch);
        for frame in tx_batch.drain(..) {
            self.ingest_guest_frame(frame);
        }

        let now = self.now_smol();
        self.iface.poll(now, &mut self.phy, &mut self.sockets);
        self.pump_tcp_sessions();
        let now = self.now_smol();
        self.iface.poll(now, &mut self.phy, &mut self.sockets);
        self.flush_egress_to_guest();

        if last_gc.elapsed() >= Duration::from_secs(5) {
            self.gc_udp_sessions();
            *last_gc = StdInstant::now();
        }

        let timeout_ms = if !block {
            0
        } else if let Some(delay) = self.iface.poll_delay(now, &self.sockets) {
            let ms = delay.total_millis();
            ms.clamp(1, 50) as i32
        } else if !self.tcp_sessions.is_empty() || !self.udp_outbound_by_token.is_empty() {
            50
        } else {
            100
        };

        if self.poller.wait(events, timeout_ms).is_err() {
            return;
        }

        for ev in events.iter().copied() {
            match ev.token & TOKEN_KIND_MASK {
                KIND_WAKE => self.wakeup.drain(),
                KIND_TCP_FWD => {
                    let idx = (ev.token & TOKEN_ID_MASK) as usize;
                    self.accept_tcp_forward(idx);
                }
                KIND_UDP_FWD => {
                    let idx = (ev.token & TOKEN_ID_MASK) as usize;
                    self.recv_udp_forward(idx);
                }
                KIND_TCP_CONN => self.handle_tcp_host_event(ev),
                KIND_UDP_OUT => self.recv_udp_outbound_reply(ev.token),
                _ => {}
            }
        }

        tx_batch.clear();
        self.channel.drain_tx_to_net(tx_batch);
        for frame in tx_batch.drain(..) {
            self.ingest_guest_frame(frame);
        }
        let now = self.now_smol();
        self.iface.poll(now, &mut self.phy, &mut self.sockets);
        self.pump_tcp_sessions();
        let now = self.now_smol();
        self.iface.poll(now, &mut self.phy, &mut self.sockets);
        self.flush_egress_to_guest();
    }

    fn flush_egress_to_guest(&mut self) {
        while let Some(frame) = self.phy.egress.pop_front() {
            if frame.len() <= VIRTIO_NET_MAX_FRAME {
                self.channel.push_rx_to_guest(frame);
            }
        }
    }

    fn ingest_guest_frame(&mut self, frame: Vec<u8>) {
        let Ok(eth) = EthernetFrame::new_checked(&frame) else {
            return;
        };
        let src_mac = eth.src_addr().0;
        if src_mac != [0; 6] && (src_mac[0] & 1) == 0 && src_mac != ROUTER_MAC {
            self.guest_mac = src_mac;
        }

        match eth.ethertype() {
            EthernetProtocol::Arp => {
                if let Ok(arp_pkt) = ArpPacket::new_checked(eth.payload()) {
                    if let Ok(ArpRepr::EthernetIpv4 {
                        operation: ArpOperation::Request,
                        target_protocol_addr,
                        ..
                    }) = ArpRepr::parse(&arp_pkt)
                    {
                        let target = std_ipv4(target_protocol_addr);
                        if target != GATEWAY_IP && target != DNS_IP {
                            return;
                        }
                    }
                }
                self.phy.ingress.push_back(frame);
            }
            EthernetProtocol::Ipv4 => {
                let Ok(ip) = Ipv4Packet::new_checked(eth.payload()) else {
                    return;
                };
                if ip.more_frags() || ip.frag_offset() != 0 || !ip.verify_checksum() {
                    return;
                }
                let src_ip = std_ipv4(ip.src_addr());
                let dst_ip = std_ipv4(ip.dst_addr());
                match ip.next_header() {
                    IpProtocol::Udp => {
                        if let Ok(udp) = UdpPacket::new_checked(ip.payload()) {
                            let src_port = udp.src_port();
                            let dst_port = udp.dst_port();
                            let payload = udp.payload().to_vec();
                            self.handle_guest_udp(src_ip, src_port, dst_ip, dst_port, &payload);
                        }
                    }
                    IpProtocol::Icmp => {
                        if dst_ip == GATEWAY_IP || dst_ip == DNS_IP {
                            self.phy.ingress.push_back(frame);
                        }
                    }
                    IpProtocol::Tcp => {
                        let Ok(tcp) = TcpPacket::new_checked(ip.payload()) else {
                            return;
                        };
                        if !tcp.verify_checksum(
                            &IpAddress::Ipv4(ip.src_addr()),
                            &IpAddress::Ipv4(ip.dst_addr()),
                        ) {
                            return;
                        }
                        if tcp.syn() && !tcp.ack() {
                            let src_port = tcp.src_port();
                            let dst_port = tcp.dst_port();
                            let seq = tcp.seq_number().0;
                            if let Some(existing) = self.tcp_sessions.values().find(|s| {
                                s.outbound_tuple == Some((src_ip, src_port, dst_ip, dst_port))
                            }) {
                                if existing.host_connecting {
                                    return;
                                }
                                self.phy.ingress.push_back(frame);
                                return;
                            }
                            if self.restrict {
                                self.emit_tcp_rst(
                                    dst_ip,
                                    dst_port,
                                    src_ip,
                                    src_port,
                                    seq.wrapping_add(1),
                                );
                                return;
                            }
                            let connect_target = if dst_ip == DNS_IP && dst_port == 53 {
                                self.dns_upstreams.first().copied().map(|ip| (ip, 53))
                            } else if is_blocked_outbound_ip(dst_ip) {
                                None
                            } else {
                                Some((dst_ip, dst_port))
                            };
                            let Some((host_ip, host_port)) = connect_target else {
                                self.emit_tcp_rst(
                                    dst_ip,
                                    dst_port,
                                    src_ip,
                                    src_port,
                                    seq.wrapping_add(1),
                                );
                                return;
                            };
                            if !self.open_outbound_tcp(
                                src_ip,
                                src_port,
                                dst_ip,
                                dst_port,
                                host_ip,
                                host_port,
                                seq.wrapping_add(1),
                                frame,
                            ) {
                                self.emit_tcp_rst(
                                    dst_ip,
                                    dst_port,
                                    src_ip,
                                    src_port,
                                    seq.wrapping_add(1),
                                );
                            }
                        } else {
                            self.phy.ingress.push_back(frame);
                        }
                    }
                    _ => {}
                }
            }
            _ => {}
        }
    }

    fn new_smol_tcp_socket() -> TcpSocket<'static> {
        let mut sock = TcpSocket::new(
            TcpSocketBuffer::new(vec![0u8; TCP_SOCKET_BUF_SIZE]),
            TcpSocketBuffer::new(vec![0u8; TCP_SOCKET_BUF_SIZE]),
        );
        sock.set_nagle_enabled(false);
        sock.set_ack_delay(None);
        sock.set_keep_alive(Some(smoltcp::time::Duration::from_secs(30)));
        sock.set_timeout(Some(smoltcp::time::Duration::from_secs(120)));
        sock
    }

    fn accept_tcp_forward(&mut self, fwd_idx: usize) {
        let Some(spec) = self.tcp_forwards.get(fwd_idx).map(|f| f.spec) else {
            return;
        };
        for _ in 0..MAX_TCP_ACCEPT_PER_TICK {
            let accept_res = self.tcp_forwards[fwd_idx].listener.accept();
            let (stream, _) = match accept_res {
                Ok(pair) => pair,
                Err(e) if e.kind() == io::ErrorKind::Interrupted => continue,
                Err(e) if e.kind() == io::ErrorKind::WouldBlock => break,
                Err(_) => break,
            };
            if self.inbound_tcp_count() >= MAX_INBOUND_TCP_SESSIONS {
                drop(stream);
                continue;
            }
            let _ = stream.set_nonblocking(true);
            let _ = stream.set_nodelay(true);

            self.seed_guest_neighbor(spec.guest_ip);
            let local_port = self.alloc_ephemeral_port();
            let mut sock = Self::new_smol_tcp_socket();
            let remote =
                IpEndpoint::new(IpAddress::Ipv4(smol_ipv4(spec.guest_ip)), spec.guest_port);
            let local = IpEndpoint::new(IpAddress::Ipv4(smol_ipv4(GATEWAY_IP)), local_port);
            if sock.connect(self.iface.context(), remote, local).is_err() {
                drop(stream);
                continue;
            }
            let token = self.alloc_tcp_token();
            let host_fd = stream.as_raw_fd();
            if self.poller.register(host_fd, token, EV_READ).is_err() {
                drop(stream);
                continue;
            }
            let handle = self.sockets.add(sock);
            self.tcp_sessions.insert(
                token,
                TcpSession {
                    token,
                    handle,
                    is_inbound: true,
                    host: Some(stream),
                    host_fd,
                    host_connecting: false,
                    connecting_since: None,
                    listen_since: None,
                    pending_syn: None,
                    syn_sent_has_data: false,
                    host_eof: false,
                    guest_eof: false,
                    pending_to_host: VecDeque::new(),
                    outbound_tuple: None,
                    time_wait_since: None,
                    closing_drain_since: None,
                },
            );
        }
    }

    #[allow(clippy::too_many_arguments)]
    fn open_outbound_tcp(
        &mut self,
        src_ip: Ipv4Addr,
        src_port: u16,
        dst_ip: Ipv4Addr,
        dst_port: u16,
        connect_ip: Ipv4Addr,
        connect_port: u16,
        rst_ack: i32,
        syn_frame: Vec<u8>,
    ) -> bool {
        if self.outbound_tcp_count() >= MAX_OUTBOUND_TCP_SESSIONS {
            return false;
        }
        let Ok((stream, connecting)) = connect_nonblocking_tcp(connect_ip, connect_port) else {
            return false;
        };
        self.register_outbound_tcp(
            src_ip,
            src_port,
            dst_ip,
            dst_port,
            stream,
            connecting,
            rst_ack,
            syn_frame,
        )
    }

    #[allow(clippy::too_many_arguments)]
    fn register_outbound_tcp(
        &mut self,
        src_ip: Ipv4Addr,
        src_port: u16,
        dst_ip: Ipv4Addr,
        dst_port: u16,
        stream: TcpStream,
        connecting: bool,
        rst_ack: i32,
        syn_frame: Vec<u8>,
    ) -> bool {
        let mut sock = Self::new_smol_tcp_socket();
        if !connecting {
            let listen_ep = IpEndpoint::new(IpAddress::Ipv4(smol_ipv4(dst_ip)), dst_port);
            if sock.listen(listen_ep).is_err() {
                return false;
            }
        }
        let token = self.alloc_tcp_token();
        let host_fd = stream.as_raw_fd();
        let interest = if connecting { EV_WRITE } else { EV_READ };
        if self.poller.register(host_fd, token, interest).is_err() {
            return false;
        }
        let handle = self.sockets.add(sock);
        let (pending_syn, listen_since) = if connecting {
            (Some((syn_frame, rst_ack)), None)
        } else {
            self.phy.ingress.push_back(syn_frame);
            let now = self.now_smol();
            self.iface.poll(now, &mut self.phy, &mut self.sockets);
            if self.sockets.get::<TcpSocket>(handle).state() == State::Listen {
                self.sockets.get_mut::<TcpSocket>(handle).abort();
                self.poller.deregister(host_fd);
                self.sockets.remove(handle);
                return false;
            }
            (None, Some(StdInstant::now()))
        };
        self.tcp_sessions.insert(
            token,
            TcpSession {
                token,
                handle,
                is_inbound: false,
                host: Some(stream),
                host_fd,
                host_connecting: connecting,
                connecting_since: if connecting {
                    Some(StdInstant::now())
                } else {
                    None
                },
                listen_since,
                pending_syn,
                syn_sent_has_data: false,
                host_eof: false,
                guest_eof: false,
                pending_to_host: VecDeque::new(),
                outbound_tuple: Some((src_ip, src_port, dst_ip, dst_port)),
                time_wait_since: None,
                closing_drain_since: None,
            },
        );
        true
    }

    fn handle_tcp_host_event(&mut self, ev: PollEvent) {
        let mut rst_target = None;
        let mut feed_syn: Option<(u64, Vec<u8>)> = None;
        {
            let Some(sess) = self.tcp_sessions.get_mut(&ev.token) else {
                return;
            };
            if sess.host_connecting && (ev.writable || ev.hup_or_err) {
                let err = get_socket_error(sess.host_fd);
                let sock = self.sockets.get_mut::<TcpSocket>(sess.handle);
                if err == 0 {
                    sess.host_connecting = false;
                    sess.connecting_since = None;
                    if let (Some((_, _, dst_ip, dst_port)), Some((syn_frame, _))) =
                        (sess.outbound_tuple, sess.pending_syn.take())
                    {
                        let listen_ep =
                            IpEndpoint::new(IpAddress::Ipv4(smol_ipv4(dst_ip)), dst_port);
                        if sock.listen(listen_ep).is_ok() {
                            sess.listen_since = Some(StdInstant::now());
                            feed_syn = Some((ev.token, syn_frame));
                        } else {
                            sock.abort();
                            self.poller.deregister(sess.host_fd);
                            sess.host = None;
                        }
                    }
                    if sess.host.is_some() {
                        let _ = self.poller.modify(sess.host_fd, sess.token, EV_READ);
                    }
                } else {
                    if let (Some((src_ip, src_port, dst_ip, dst_port)), Some((_, rst_ack))) =
                        (sess.outbound_tuple, sess.pending_syn.take())
                    {
                        rst_target = Some((dst_ip, dst_port, src_ip, src_port, rst_ack));
                    }
                    sock.abort();
                    self.poller.deregister(sess.host_fd);
                    sess.host = None;
                    sess.host_connecting = false;
                    sess.connecting_since = None;
                }
            } else {
                if ev.hup_or_err && sess.host.is_some() {
                    let err = get_socket_error(sess.host_fd);
                    if err != 0 {
                        self.sockets.get_mut::<TcpSocket>(sess.handle).abort();
                        self.poller.deregister(sess.host_fd);
                        sess.host = None;
                        sess.pending_to_host.clear();
                        return;
                    }
                }
                if ev.writable && !sess.pending_to_host.is_empty() {
                    if let Some(ref mut stream) = sess.host {
                        while !sess.pending_to_host.is_empty() {
                            let (a, _) = sess.pending_to_host.as_slices();
                            match stream.write(a) {
                                Ok(0) => {
                                    self.sockets.get_mut::<TcpSocket>(sess.handle).abort();
                                    self.poller.deregister(sess.host_fd);
                                    sess.host = None;
                                    sess.pending_to_host.clear();
                                    return;
                                }
                                Ok(n) => {
                                    sess.pending_to_host.drain(..n);
                                }
                                Err(e) if e.kind() == io::ErrorKind::Interrupted => continue,
                                Err(e) if e.kind() == io::ErrorKind::WouldBlock => break,
                                Err(_) => {
                                    self.sockets.get_mut::<TcpSocket>(sess.handle).abort();
                                    self.poller.deregister(sess.host_fd);
                                    sess.host = None;
                                    sess.pending_to_host.clear();
                                    return;
                                }
                            }
                        }
                    }
                }
            }
        }
        if let Some((tok, syn_frame)) = feed_syn {
            self.phy.ingress.push_back(syn_frame);
            let now = self.now_smol();
            self.iface.poll(now, &mut self.phy, &mut self.sockets);
            let still_listen = self
                .tcp_sessions
                .get(&tok)
                .is_some_and(|s| self.sockets.get::<TcpSocket>(s.handle).state() == State::Listen);
            if still_listen {
                if let Some(sess) = self.tcp_sessions.remove(&tok) {
                    self.sockets.get_mut::<TcpSocket>(sess.handle).abort();
                    if sess.host.is_some() {
                        self.poller.deregister(sess.host_fd);
                    }
                    self.sockets.remove(sess.handle);
                }
            }
        }
        if let Some((src_ip, src_port, dst_ip, dst_port, ack)) = rst_target {
            self.emit_tcp_rst(src_ip, src_port, dst_ip, dst_port, ack);
        }
    }

    fn pump_tcp_sessions(&mut self) {
        let tokens: Vec<u64> = self.tcp_sessions.keys().copied().collect();
        let mut dead = Vec::new();
        let mut rst_targets = Vec::new();
        let mut rx_buf = [0u8; 16384];

        for token in tokens {
            let Some(sess) = self.tcp_sessions.get_mut(&token) else {
                continue;
            };
            let sock = self.sockets.get_mut::<TcpSocket>(sess.handle);
            let state = sock.state();

            if sess.host_connecting {
                if sess
                    .connecting_since
                    .is_some_and(|t| t.elapsed() >= self.connect_timeout)
                {
                    if let (Some((src_ip, src_port, dst_ip, dst_port)), Some((_, rst_ack))) =
                        (sess.outbound_tuple, sess.pending_syn.take())
                    {
                        rst_targets.push((dst_ip, dst_port, src_ip, src_port, rst_ack));
                    }
                    sock.abort();
                    dead.push(token);
                }
                continue;
            }

            if state == State::Listen {
                if sess
                    .listen_since
                    .is_none_or(|t| t.elapsed() >= TCP_LISTEN_TIMEOUT)
                {
                    sock.abort();
                    dead.push(token);
                }
                continue;
            }

            if state == State::Closed {
                if sess.host.is_some() && sock.can_recv() {
                    while let Ok(n) = sock.recv_slice(&mut rx_buf) {
                        if n == 0 {
                            break;
                        }
                        sess.pending_to_host.extend(&rx_buf[..n]);
                    }
                }
                if let Some(ref mut stream) = sess.host {
                    let mut failed = false;
                    while !sess.pending_to_host.is_empty() {
                        let (a, _) = sess.pending_to_host.as_slices();
                        match stream.write(a) {
                            Ok(0) => {
                                failed = true;
                                break;
                            }
                            Ok(n) => {
                                sess.pending_to_host.drain(..n);
                            }
                            Err(e) if e.kind() == io::ErrorKind::Interrupted => continue,
                            Err(e) if e.kind() == io::ErrorKind::WouldBlock => break,
                            Err(_) => {
                                failed = true;
                                break;
                            }
                        }
                    }
                    if !failed && !sess.pending_to_host.is_empty() {
                        let since = *sess.closing_drain_since.get_or_insert_with(StdInstant::now);
                        if since.elapsed() < TCP_CLOSE_DRAIN_TIMEOUT {
                            let _ = self.poller.modify(sess.host_fd, sess.token, EV_WRITE);
                            continue;
                        }
                    }
                }
                dead.push(token);
                continue;
            }

            if !sess.pending_to_host.is_empty() {
                if let Some(ref mut stream) = sess.host {
                    let mut failed = false;
                    while !sess.pending_to_host.is_empty() {
                        let (a, _) = sess.pending_to_host.as_slices();
                        match stream.write(a) {
                            Ok(0) => {
                                failed = true;
                                break;
                            }
                            Ok(n) => {
                                sess.pending_to_host.drain(..n);
                            }
                            Err(e) if e.kind() == io::ErrorKind::Interrupted => continue,
                            Err(e) if e.kind() == io::ErrorKind::WouldBlock => break,
                            Err(_) => {
                                failed = true;
                                break;
                            }
                        }
                    }
                    if failed {
                        sock.abort();
                        dead.push(token);
                        continue;
                    }
                }
            }

            if sess.pending_to_host.is_empty() && sess.host.is_some() {
                while sock.can_recv() {
                    let Ok(n) = sock.recv_slice(&mut rx_buf) else {
                        break;
                    };
                    if n == 0 {
                        break;
                    }
                    let stream = sess.host.as_mut().unwrap();
                    let mut written = 0usize;
                    let mut failed = false;
                    while written < n {
                        match stream.write(&rx_buf[written..n]) {
                            Ok(0) => {
                                failed = true;
                                break;
                            }
                            Ok(w) => written += w,
                            Err(e) if e.kind() == io::ErrorKind::Interrupted => continue,
                            Err(e) if e.kind() == io::ErrorKind::WouldBlock => {
                                sess.pending_to_host.extend(&rx_buf[written..n]);
                                break;
                            }
                            Err(_) => {
                                failed = true;
                                break;
                            }
                        }
                    }
                    if failed {
                        sock.abort();
                        dead.push(token);
                        break;
                    }
                    if !sess.pending_to_host.is_empty() {
                        break;
                    }
                }
            }
            if dead.last() == Some(&token) {
                continue;
            }

            if !sess.guest_eof
                && sess.pending_to_host.is_empty()
                && !sock.can_recv()
                && matches!(
                    sock.state(),
                    State::CloseWait | State::LastAck | State::Closing | State::TimeWait
                )
            {
                if let Some(ref stream) = sess.host {
                    let _ = stream.shutdown(Shutdown::Write);
                }
                sess.guest_eof = true;
            }

            if !sess.host_eof {
                if let Some(ref mut stream) = sess.host {
                    if state == State::SynSent {
                        if !sess.syn_sent_has_data {
                            let mut peek = [0u8; 1];
                            loop {
                                match stream.peek(&mut peek) {
                                    Ok(0) => {
                                        sock.abort();
                                        dead.push(token);
                                        break;
                                    }
                                    Ok(_) => {
                                        sess.syn_sent_has_data = true;
                                        break;
                                    }
                                    Err(e) if e.kind() == io::ErrorKind::Interrupted => continue,
                                    Err(e) if e.kind() == io::ErrorKind::WouldBlock => break,
                                    Err(_) => {
                                        sock.abort();
                                        dead.push(token);
                                        break;
                                    }
                                }
                            }
                        }
                    } else if sock.can_send() {
                        sess.syn_sent_has_data = false;
                        loop {
                            let avail = sock.send_capacity().saturating_sub(sock.send_queue());
                            if avail == 0 {
                                break;
                            }
                            let want = avail.min(rx_buf.len());
                            match stream.read(&mut rx_buf[..want]) {
                                Ok(0) => {
                                    sess.host_eof = true;
                                    break;
                                }
                                Ok(n) => {
                                    if sock.send_slice(&rx_buf[..n]).is_err() {
                                        sock.abort();
                                        dead.push(token);
                                        break;
                                    }
                                }
                                Err(e) if e.kind() == io::ErrorKind::Interrupted => continue,
                                Err(e) if e.kind() == io::ErrorKind::WouldBlock => break,
                                Err(_) => {
                                    sock.abort();
                                    dead.push(token);
                                    break;
                                }
                            }
                        }
                    }
                }
            }
            if dead.last() == Some(&token) {
                continue;
            }

            if sess.host_eof
                && sock.send_queue() == 0
                && matches!(sock.state(), State::Established | State::CloseWait)
            {
                sock.close();
            }

            if sess.host.is_some() {
                let want_read = !sess.host_eof
                    && ((sock.state() == State::SynSent && !sess.syn_sent_has_data)
                        || (sock.can_send() && sock.send_capacity() > sock.send_queue()));
                let want_write = !sess.pending_to_host.is_empty();
                let interest = (if want_read { EV_READ } else { 0 })
                    | (if want_write { EV_WRITE } else { 0 });
                let _ = self.poller.modify(sess.host_fd, sess.token, interest);
            }

            if sess.host_eof && sess.guest_eof && sess.pending_to_host.is_empty() {
                if sess.host.is_some() {
                    self.poller.deregister(sess.host_fd);
                    sess.host = None;
                }
                if sock.state() == State::TimeWait {
                    let since = *sess.time_wait_since.get_or_insert_with(StdInstant::now);
                    if since.elapsed() >= Duration::from_secs(2) {
                        sock.abort();
                        dead.push(token);
                    }
                }
            }
        }

        for token in dead {
            if let Some(sess) = self.tcp_sessions.remove(&token) {
                if sess.host.is_some() {
                    self.poller.deregister(sess.host_fd);
                }
                self.sockets.remove(sess.handle);
            }
        }
        for (src_ip, src_port, dst_ip, dst_port, ack) in rst_targets {
            self.emit_tcp_rst(src_ip, src_port, dst_ip, dst_port, ack);
        }
    }

    fn recv_udp_forward(&mut self, fwd_idx: usize) {
        let Some(spec) = self.udp_forwards.get(fwd_idx).map(|f| f.spec) else {
            return;
        };
        let mut buf = [0u8; 2048];
        for _ in 0..MAX_UDP_RECV_PER_TICK {
            let res = self.udp_forwards[fwd_idx].socket.recv_from(&mut buf);
            let (n, peer) = match res {
                Ok((n, std::net::SocketAddr::V4(v4))) => (n, v4),
                Ok(_) => continue,
                Err(e) if e.kind() == io::ErrorKind::Interrupted => continue,
                Err(e) if e.kind() == io::ErrorKind::WouldBlock => break,
                Err(_) => break,
            };
            let ephem = if let Some(&port) = self.udp_inbound_by_peer.get(&(fwd_idx, peer)) {
                if let Some(sess) = self.udp_inbound_by_ephem.get_mut(&port) {
                    sess.last_active = StdInstant::now();
                }
                port
            } else {
                if self.udp_inbound_by_ephem.len() >= MAX_UDP_SESSIONS {
                    self.evict_oldest_inbound_udp();
                }
                let port = self.alloc_ephemeral_port();
                self.udp_inbound_by_ephem.insert(
                    port,
                    UdpInboundSession {
                        fwd_idx,
                        host_peer: peer,
                        guest_ip: spec.guest_ip,
                        guest_port: spec.guest_port,
                        last_active: StdInstant::now(),
                    },
                );
                self.udp_inbound_by_peer.insert((fwd_idx, peer), port);
                port
            };
            self.emit_udp_frame(GATEWAY_IP, ephem, spec.guest_ip, spec.guest_port, &buf[..n]);
        }
    }

    fn handle_guest_udp(
        &mut self,
        src_ip: Ipv4Addr,
        src_port: u16,
        dst_ip: Ipv4Addr,
        dst_port: u16,
        payload: &[u8],
    ) {
        if src_port == 68 && dst_port == 67 {
            if let Some((reply_dst_ip, reply_dst_mac, reply)) =
                build_dhcp_reply(payload, self.restrict)
            {
                self.emit_udp_frame_to(
                    GATEWAY_IP,
                    67,
                    reply_dst_ip,
                    68,
                    reply_dst_mac,
                    &reply,
                );
            }
            return;
        }

        if dst_ip == GATEWAY_IP {
            if let Some(sess) = self.udp_inbound_by_ephem.get_mut(&dst_port) {
                if sess.guest_ip == src_ip && sess.guest_port == src_port {
                    sess.last_active = StdInstant::now();
                    let fwd_idx = sess.fwd_idx;
                    let peer = sess.host_peer;
                    if let Some(fwd) = self.udp_forwards.get(fwd_idx) {
                        loop {
                            match fwd.socket.send_to(payload, peer) {
                                Err(e) if e.kind() == io::ErrorKind::Interrupted => continue,
                                _ => break,
                            }
                        }
                    }
                }
            }
            return;
        }

        if self.restrict || src_ip.is_unspecified() {
            return;
        }

        let is_dns = dst_ip == DNS_IP && dst_port == 53;
        let targets: Vec<SocketAddrV4> = if is_dns {
            self.dns_upstreams
                .iter()
                .map(|&ip| SocketAddrV4::new(ip, 53))
                .collect()
        } else {
            if is_blocked_outbound_ip(dst_ip) {
                return;
            }
            vec![SocketAddrV4::new(dst_ip, dst_port)]
        };
        if targets.is_empty() {
            return;
        }

        let key = (src_ip, src_port, dst_ip, dst_port);
        if let Some(&tok) = self.udp_outbound_by_key.get(&key) {
            if let Some(sess) = self.udp_outbound_by_token.get_mut(&tok) {
                sess.last_active = StdInstant::now();
                let sent_ok = loop {
                    match sess.socket.send(payload) {
                        Ok(_) => break true,
                        Err(e) if e.kind() == io::ErrorKind::Interrupted => continue,
                        Err(e) if e.kind() == io::ErrorKind::WouldBlock => break true,
                        Err(_) => break false,
                    }
                };
                if sent_ok {
                    return;
                }
            }
            if let Some(old) = self.udp_outbound_by_token.remove(&tok) {
                self.poller.deregister(old.socket.as_raw_fd());
            }
            self.udp_outbound_by_key.remove(&key);
        }

        for target_addr in targets {
            if self.udp_outbound_by_token.len() >= MAX_UDP_SESSIONS {
                self.evict_oldest_outbound_udp();
            }
            let bind_ip = if target_addr.ip().is_loopback() {
                Ipv4Addr::LOCALHOST
            } else {
                Ipv4Addr::UNSPECIFIED
            };
            let Ok(sock) = UdpSocket::bind(SocketAddrV4::new(bind_ip, 0)) else {
                continue;
            };
            if sock.set_nonblocking(true).is_err() || sock.connect(target_addr).is_err() {
                continue;
            }
            let send_ok = loop {
                match sock.send(payload) {
                    Ok(_) => break true,
                    Err(e) if e.kind() == io::ErrorKind::Interrupted => continue,
                    Err(e) if e.kind() == io::ErrorKind::WouldBlock => break true,
                    Err(_) => break false,
                }
            };
            if !send_ok {
                continue;
            }
            let tok = self.alloc_udp_token();
            if self.poller.register(sock.as_raw_fd(), tok, EV_READ).is_err() {
                continue;
            }
            self.udp_outbound_by_key.insert(key, tok);
            self.udp_outbound_by_token.insert(
                tok,
                UdpOutboundSession {
                    guest_src_ip: src_ip,
                    guest_src_port: src_port,
                    virtual_dst_ip: dst_ip,
                    virtual_dst_port: dst_port,
                    expected_peer: target_addr,
                    socket: sock,
                    last_active: StdInstant::now(),
                },
            );
            return;
        }
    }

    fn recv_udp_outbound_reply(&mut self, token: u64) {
        let mut buf = [0u8; 2048];
        let mut replies = Vec::new();
        if let Some(sess) = self.udp_outbound_by_token.get_mut(&token) {
            for _ in 0..MAX_UDP_RECV_PER_TICK {
                match sess.socket.recv_from(&mut buf) {
                    Ok((n, std::net::SocketAddr::V4(peer))) => {
                        if peer != sess.expected_peer {
                            continue;
                        }
                        sess.last_active = StdInstant::now();
                        replies.push((
                            sess.virtual_dst_ip,
                            sess.virtual_dst_port,
                            sess.guest_src_ip,
                            sess.guest_src_port,
                            buf[..n].to_vec(),
                        ));
                    }
                    Ok(_) => continue,
                    Err(e) if e.kind() == io::ErrorKind::Interrupted => continue,
                    Err(e) if e.kind() == io::ErrorKind::WouldBlock => break,
                    Err(_) => break,
                }
            }
        }
        for (src_ip, src_port, dst_ip, dst_port, payload) in replies {
            self.emit_udp_frame(src_ip, src_port, dst_ip, dst_port, &payload);
        }
    }

    fn evict_oldest_inbound_udp(&mut self) {
        if let Some((&oldest_port, _)) = self
            .udp_inbound_by_ephem
            .iter()
            .min_by_key(|(_, s)| s.last_active)
        {
            if let Some(s) = self.udp_inbound_by_ephem.remove(&oldest_port) {
                self.udp_inbound_by_peer.remove(&(s.fwd_idx, s.host_peer));
            }
        }
    }

    fn evict_oldest_outbound_udp(&mut self) {
        if let Some((&oldest_tok, _)) = self
            .udp_outbound_by_token
            .iter()
            .min_by_key(|(_, s)| s.last_active)
        {
            if let Some(s) = self.udp_outbound_by_token.remove(&oldest_tok) {
                self.poller.deregister(s.socket.as_raw_fd());
                self.udp_outbound_by_key.remove(&(
                    s.guest_src_ip,
                    s.guest_src_port,
                    s.virtual_dst_ip,
                    s.virtual_dst_port,
                ));
            }
        }
    }

    fn gc_udp_sessions(&mut self) {
        let now = StdInstant::now();
        let expired_in: Vec<u16> = self
            .udp_inbound_by_ephem
            .iter()
            .filter(|(_, s)| now.duration_since(s.last_active) > UDP_IDLE_TIMEOUT)
            .map(|(&p, _)| p)
            .collect();
        for p in expired_in {
            if let Some(s) = self.udp_inbound_by_ephem.remove(&p) {
                self.udp_inbound_by_peer.remove(&(s.fwd_idx, s.host_peer));
            }
        }

        let expired_out: Vec<u64> = self
            .udp_outbound_by_token
            .iter()
            .filter(|(_, s)| now.duration_since(s.last_active) > UDP_IDLE_TIMEOUT)
            .map(|(&t, _)| t)
            .collect();
        for t in expired_out {
            if let Some(s) = self.udp_outbound_by_token.remove(&t) {
                self.poller.deregister(s.socket.as_raw_fd());
                self.udp_outbound_by_key.remove(&(
                    s.guest_src_ip,
                    s.guest_src_port,
                    s.virtual_dst_ip,
                    s.virtual_dst_port,
                ));
            }
        }
    }

    fn next_ip_id(&mut self) -> u16 {
        let id = self.ip_id;
        self.ip_id = self.ip_id.wrapping_add(1);
        id
    }

    fn emit_udp_frame(
        &mut self,
        src_ip: Ipv4Addr,
        src_port: u16,
        dst_ip: Ipv4Addr,
        dst_port: u16,
        payload: &[u8],
    ) {
        self.emit_udp_frame_to(src_ip, src_port, dst_ip, dst_port, self.guest_mac, payload);
    }

    fn emit_udp_frame_to(
        &mut self,
        src_ip: Ipv4Addr,
        src_port: u16,
        dst_ip: Ipv4Addr,
        dst_port: u16,
        dst_mac: [u8; 6],
        payload: &[u8],
    ) {
        let udp_repr = UdpRepr { src_port, dst_port };
        let ip_repr = Ipv4Repr {
            src_addr: smol_ipv4(src_ip),
            dst_addr: smol_ipv4(dst_ip),
            next_header: IpProtocol::Udp,
            payload_len: udp_repr.header_len() + payload.len(),
            hop_limit: 64,
        };
        let eth_repr = EthernetRepr {
            src_addr: EthernetAddress(ROUTER_MAC),
            dst_addr: EthernetAddress(dst_mac),
            ethertype: EthernetProtocol::Ipv4,
        };
        let total =
            eth_repr.buffer_len() + ip_repr.buffer_len() + udp_repr.header_len() + payload.len();
        if total > MAX_FRAME_LEN {
            return;
        }
        let ip_id = self.next_ip_id();
        let mut buf = vec![0u8; total];
        let mut eth = EthernetFrame::new_unchecked(&mut buf);
        eth_repr.emit(&mut eth);
        let mut ip = Ipv4Packet::new_unchecked(eth.payload_mut());
        ip_repr.emit(&mut ip, &ChecksumCapabilities::default());
        ip.set_ident(ip_id);
        ip.fill_checksum();
        let mut udp = UdpPacket::new_unchecked(ip.payload_mut());
        udp_repr.emit(
            &mut udp,
            &IpAddress::Ipv4(ip_repr.src_addr),
            &IpAddress::Ipv4(ip_repr.dst_addr),
            payload.len(),
            |p| p.copy_from_slice(payload),
            &ChecksumCapabilities::default(),
        );
        self.phy.egress.push_back(buf);
    }

    fn emit_tcp_rst(
        &mut self,
        src_ip: Ipv4Addr,
        src_port: u16,
        dst_ip: Ipv4Addr,
        dst_port: u16,
        ack_num: i32,
    ) {
        let tcp_repr = TcpRepr {
            src_port,
            dst_port,
            control: TcpControl::Rst,
            seq_number: smoltcp::wire::TcpSeqNumber(0),
            ack_number: Some(smoltcp::wire::TcpSeqNumber(ack_num)),
            window_len: 0,
            window_scale: None,
            max_seg_size: None,
            sack_permitted: false,
            sack_ranges: [None, None, None],
            timestamp: None,
            payload: &[],
        };
        let ip_repr = Ipv4Repr {
            src_addr: smol_ipv4(src_ip),
            dst_addr: smol_ipv4(dst_ip),
            next_header: IpProtocol::Tcp,
            payload_len: tcp_repr.buffer_len(),
            hop_limit: 64,
        };
        let eth_repr = EthernetRepr {
            src_addr: EthernetAddress(ROUTER_MAC),
            dst_addr: EthernetAddress(self.guest_mac),
            ethertype: EthernetProtocol::Ipv4,
        };
        let total = eth_repr.buffer_len() + ip_repr.buffer_len() + tcp_repr.buffer_len();
        let ip_id = self.next_ip_id();
        let mut buf = vec![0u8; total];
        let mut eth = EthernetFrame::new_unchecked(&mut buf);
        eth_repr.emit(&mut eth);
        let mut ip = Ipv4Packet::new_unchecked(eth.payload_mut());
        ip_repr.emit(&mut ip, &ChecksumCapabilities::default());
        ip.set_ident(ip_id);
        ip.fill_checksum();
        let mut tcp = TcpPacket::new_unchecked(ip.payload_mut());
        tcp_repr.emit(
            &mut tcp,
            &IpAddress::Ipv4(ip_repr.src_addr),
            &IpAddress::Ipv4(ip_repr.dst_addr),
            &ChecksumCapabilities::default(),
        );
        self.phy.egress.push_back(buf);
    }
}

fn connect_nonblocking_tcp(dst_ip: Ipv4Addr, dst_port: u16) -> io::Result<(TcpStream, bool)> {
    #[cfg(target_os = "linux")]
    let fd = unsafe {
        libc::socket(
            libc::AF_INET,
            libc::SOCK_STREAM | libc::SOCK_NONBLOCK | libc::SOCK_CLOEXEC,
            0,
        )
    };
    #[cfg(not(target_os = "linux"))]
    let fd = unsafe { libc::socket(libc::AF_INET, libc::SOCK_STREAM, 0) };
    if fd < 0 {
        return Err(io::Error::last_os_error());
    }
    #[cfg(not(target_os = "linux"))]
    unsafe {
        let fl = libc::fcntl(fd, libc::F_GETFL);
        libc::fcntl(fd, libc::F_SETFL, fl | libc::O_NONBLOCK);
        libc::fcntl(fd, libc::F_SETFD, libc::FD_CLOEXEC);
    }
    let one: libc::c_int = 1;
    unsafe {
        libc::setsockopt(
            fd,
            libc::IPPROTO_TCP,
            libc::TCP_NODELAY,
            (&one as *const libc::c_int).cast::<libc::c_void>(),
            std::mem::size_of::<libc::c_int>() as libc::socklen_t,
        );
    }

    let mut sa: libc::sockaddr_in = unsafe { std::mem::zeroed() };
    sa.sin_family = libc::AF_INET as libc::sa_family_t;
    sa.sin_port = dst_port.to_be();
    sa.sin_addr = libc::in_addr {
        s_addr: u32::from_ne_bytes(dst_ip.octets()),
    };

    let rc = unsafe {
        libc::connect(
            fd,
            (&sa as *const libc::sockaddr_in).cast::<libc::sockaddr>(),
            std::mem::size_of::<libc::sockaddr_in>() as libc::socklen_t,
        )
    };
    if rc == 0 {
        return Ok((unsafe { TcpStream::from_raw_fd(fd) }, false));
    }
    let err = io::Error::last_os_error();
    if err.raw_os_error() == Some(libc::EINPROGRESS) {
        return Ok((unsafe { TcpStream::from_raw_fd(fd) }, true));
    }
    unsafe {
        libc::close(fd);
    }
    Err(err)
}

fn get_socket_error(fd: RawFd) -> i32 {
    let mut err: libc::c_int = 0;
    let mut len = std::mem::size_of::<libc::c_int>() as libc::socklen_t;
    let rc = unsafe {
        libc::getsockopt(
            fd,
            libc::SOL_SOCKET,
            libc::SO_ERROR,
            (&mut err as *mut libc::c_int).cast::<libc::c_void>(),
            &mut len,
        )
    };
    if rc != 0 {
        return libc::EIO;
    }
    err
}

const DHCP_MAGIC_COOKIE: [u8; 4] = [0x63, 0x82, 0x53, 0x63];
const DHCP_DISCOVER: u8 = 1;
const DHCP_OFFER: u8 = 2;
const DHCP_REQUEST: u8 = 3;
const DHCP_ACK: u8 = 5;
const DHCP_NAK: u8 = 6;

fn build_dhcp_reply(req: &[u8], restrict: bool) -> Option<(Ipv4Addr, [u8; 6], Vec<u8>)> {
    if req.len() < 240 || req[0] != 1 || req[1] != 1 || req[2] != 6 {
        return None;
    }
    if req[236..240] != DHCP_MAGIC_COOKIE {
        return None;
    }

    let mut msg_type = 0u8;
    let mut requested_ip: Option<Ipv4Addr> = None;
    let mut i = 240;
    while i < req.len() {
        let opt = req[i];
        if opt == 255 {
            break;
        }
        if opt == 0 {
            i += 1;
            continue;
        }
        if i + 1 >= req.len() {
            return None;
        }
        let len = req[i + 1] as usize;
        if i + 2 + len > req.len() {
            return None;
        }
        if opt == 53 && len == 1 {
            msg_type = req[i + 2];
        } else if opt == 50 && len == 4 {
            requested_ip = Some(Ipv4Addr::new(
                req[i + 2],
                req[i + 3],
                req[i + 4],
                req[i + 5],
            ));
        }
        i += 2 + len;
    }

    let reply_type = match msg_type {
        DHCP_DISCOVER => DHCP_OFFER,
        DHCP_REQUEST => {
            if let Some(ip) = requested_ip {
                if ip != GUEST_IP {
                    DHCP_NAK
                } else {
                    DHCP_ACK
                }
            } else {
                DHCP_ACK
            }
        }
        _ => return None,
    };

    let mut chaddr = [0u8; 6];
    chaddr.copy_from_slice(&req[28..34]);
    let flags = u16::from_be_bytes([req[10], req[11]]);
    let broadcast = (flags & 0x8000) != 0 || reply_type == DHCP_NAK;

    let mut out = vec![0u8; 300];
    out[0] = 2;
    out[1] = 1;
    out[2] = 6;
    out[4..8].copy_from_slice(&req[4..8]);
    out[10..12].copy_from_slice(&req[10..12]);
    if reply_type != DHCP_NAK {
        out[16..20].copy_from_slice(&GUEST_IP.octets());
    }
    out[20..24].copy_from_slice(&GATEWAY_IP.octets());
    out[28..34].copy_from_slice(&chaddr);
    out[236..240].copy_from_slice(&DHCP_MAGIC_COOKIE);

    let mut p = 240;
    out[p..p + 3].copy_from_slice(&[53, 1, reply_type]);
    p += 3;
    out[p..p + 2].copy_from_slice(&[54, 4]);
    out[p + 2..p + 6].copy_from_slice(&GATEWAY_IP.octets());
    p += 6;
    if reply_type != DHCP_NAK {
        out[p..p + 2].copy_from_slice(&[51, 4]);
        out[p + 2..p + 6].copy_from_slice(&86400u32.to_be_bytes());
        p += 6;
        out[p..p + 2].copy_from_slice(&[1, 4]);
        out[p + 2..p + 6].copy_from_slice(&[255, 255, 255, 0]);
        p += 6;
        out[p..p + 2].copy_from_slice(&[3, 4]);
        out[p + 2..p + 6].copy_from_slice(&GATEWAY_IP.octets());
        p += 6;
        if !restrict {
            out[p..p + 2].copy_from_slice(&[6, 4]);
            out[p + 2..p + 6].copy_from_slice(&DNS_IP.octets());
            p += 6;
        }
    }
    out[p] = 255;

    let dst_ip = if broadcast {
        Ipv4Addr::BROADCAST
    } else {
        GUEST_IP
    };
    let dst_mac = if broadcast { [0xff; 6] } else { chaddr };
    Some((dst_ip, dst_mac, out))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn make_udp_eth(
        src_mac: [u8; 6],
        dst_mac: [u8; 6],
        src_ip: Ipv4Addr,
        src_port: u16,
        dst_ip: Ipv4Addr,
        dst_port: u16,
        payload: &[u8],
    ) -> Vec<u8> {
        let udp_repr = UdpRepr { src_port, dst_port };
        let ip_repr = Ipv4Repr {
            src_addr: smol_ipv4(src_ip),
            dst_addr: smol_ipv4(dst_ip),
            next_header: IpProtocol::Udp,
            payload_len: udp_repr.header_len() + payload.len(),
            hop_limit: 64,
        };
        let eth_repr = EthernetRepr {
            src_addr: EthernetAddress(src_mac),
            dst_addr: EthernetAddress(dst_mac),
            ethertype: EthernetProtocol::Ipv4,
        };
        let total =
            eth_repr.buffer_len() + ip_repr.buffer_len() + udp_repr.header_len() + payload.len();
        let mut buf = vec![0u8; total];
        let mut eth = EthernetFrame::new_unchecked(&mut buf);
        eth_repr.emit(&mut eth);
        let mut ip = Ipv4Packet::new_unchecked(eth.payload_mut());
        ip_repr.emit(&mut ip, &ChecksumCapabilities::default());
        let mut udp = UdpPacket::new_unchecked(ip.payload_mut());
        udp_repr.emit(
            &mut udp,
            &IpAddress::Ipv4(ip_repr.src_addr),
            &IpAddress::Ipv4(ip_repr.dst_addr),
            payload.len(),
            |p| p.copy_from_slice(payload),
            &ChecksumCapabilities::default(),
        );
        buf
    }

    fn make_tcp_syn_eth(
        src_ip: Ipv4Addr,
        src_port: u16,
        dst_ip: Ipv4Addr,
        dst_port: u16,
        seq: i32,
    ) -> Vec<u8> {
        let syn_repr = TcpRepr {
            src_port,
            dst_port,
            control: TcpControl::Syn,
            seq_number: smoltcp::wire::TcpSeqNumber(seq),
            ack_number: None,
            window_len: 4096,
            window_scale: None,
            max_seg_size: Some(1460),
            sack_permitted: false,
            sack_ranges: [None; 3],
            timestamp: None,
            payload: &[],
        };
        let ip_repr = Ipv4Repr {
            src_addr: smol_ipv4(src_ip),
            dst_addr: smol_ipv4(dst_ip),
            next_header: IpProtocol::Tcp,
            payload_len: syn_repr.buffer_len(),
            hop_limit: 64,
        };
        let mut frame = vec![0u8; 14 + ip_repr.buffer_len() + syn_repr.buffer_len()];
        let mut eth = EthernetFrame::new_unchecked(&mut frame);
        EthernetRepr {
            src_addr: EthernetAddress(DEFAULT_GUEST_MAC),
            dst_addr: EthernetAddress(ROUTER_MAC),
            ethertype: EthernetProtocol::Ipv4,
        }
        .emit(&mut eth);
        let mut ip = Ipv4Packet::new_unchecked(eth.payload_mut());
        ip_repr.emit(&mut ip, &ChecksumCapabilities::default());
        syn_repr.emit(
            &mut TcpPacket::new_unchecked(ip.payload_mut()),
            &IpAddress::Ipv4(ip_repr.src_addr),
            &IpAddress::Ipv4(ip_repr.dst_addr),
            &ChecksumCapabilities::default(),
        );
        frame
    }

    #[test]
    fn parse_mac_forward_resolv_and_blocklist() {
        assert_eq!(
            parse_mac("02:dc:72:aa:bb:cc").unwrap(),
            [0x02, 0xdc, 0x72, 0xaa, 0xbb, 0xcc]
        );
        assert!(parse_mac("00:00:00:00:00:00").is_err());
        assert!(parse_mac("01:00:5e:00:00:01").is_err());
        assert!(parse_mac("bad").is_err());

        let f4 = parse_forward("tcp:127.0.0.1:2222:1023").unwrap();
        assert_eq!(f4.proto, ForwardProto::Tcp);
        assert_eq!(f4.host_ip, Ipv4Addr::LOCALHOST);
        assert_eq!(f4.host_port, 2222);
        assert_eq!(f4.guest_ip, GUEST_IP);
        assert_eq!(f4.guest_port, 1023);

        let f5 = parse_forward("udp:127.0.0.1:5353:10.0.2.15:9999").unwrap();
        assert_eq!(f5.proto, ForwardProto::Udp);
        assert_eq!(f5.host_port, 5353);
        assert_eq!(f5.guest_port, 9999);

        assert!(is_blocked_outbound_ip(Ipv4Addr::new(0, 1, 2, 3)));
        assert!(is_blocked_outbound_ip(Ipv4Addr::new(240, 0, 0, 1)));
        assert!(is_blocked_outbound_ip(Ipv4Addr::LOCALHOST));
        assert!(!is_blocked_outbound_ip(Ipv4Addr::new(1, 1, 1, 1)));

        let dns = parse_resolv_conf_str("nameserver1.2.3.4\nnameserverfoo 9.9.9.9\nnameserver 8.8.4.4\n");
        assert_eq!(dns, vec![Ipv4Addr::new(8, 8, 4, 4)]);
    }

    #[test]
    fn arp_and_dhcp_offer_ack_nak_work() {
        let ch = NetChannel::new();
        let mut eng = NetEngine::bind(Arc::clone(&ch), DEFAULT_GUEST_MAC, &[], true).unwrap();
        let mut ev = Vec::new();
        let mut tx = Vec::new();
        let mut gc = StdInstant::now();

        let arp_repr = ArpRepr::EthernetIpv4 {
            operation: ArpOperation::Request,
            source_hardware_addr: EthernetAddress(DEFAULT_GUEST_MAC),
            source_protocol_addr: smol_ipv4(GUEST_IP),
            target_hardware_addr: EthernetAddress([0; 6]),
            target_protocol_addr: smol_ipv4(GATEWAY_IP),
        };
        let mut arp_buf = vec![0u8; 14 + arp_repr.buffer_len()];
        let mut eth = EthernetFrame::new_unchecked(&mut arp_buf);
        EthernetRepr {
            src_addr: EthernetAddress(DEFAULT_GUEST_MAC),
            dst_addr: EthernetAddress([0xff; 6]),
            ethertype: EthernetProtocol::Arp,
        }
        .emit(&mut eth);
        arp_repr.emit(&mut ArpPacket::new_unchecked(eth.payload_mut()));
        ch.push_tx_from_guest(arp_buf);

        eng.step(&mut ev, &mut tx, &mut gc, false);
        let reply = ch.pop_rx_for_guest().expect("expected ARP reply");
        let reth = EthernetFrame::new_checked(&reply).unwrap();
        assert_eq!(reth.ethertype(), EthernetProtocol::Arp);
        let rarp = ArpRepr::parse(&ArpPacket::new_checked(reth.payload()).unwrap()).unwrap();
        match rarp {
            ArpRepr::EthernetIpv4 {
                operation,
                source_hardware_addr,
                source_protocol_addr,
                ..
            } => {
                assert_eq!(operation, ArpOperation::Reply);
                assert_eq!(source_hardware_addr.0, ROUTER_MAC);
                assert_eq!(source_protocol_addr, smol_ipv4(GATEWAY_IP));
            }
            _ => panic!("unexpected ARP repr"),
        }

        let dad_repr = ArpRepr::EthernetIpv4 {
            operation: ArpOperation::Request,
            source_hardware_addr: EthernetAddress(DEFAULT_GUEST_MAC),
            source_protocol_addr: Ipv4Address::UNSPECIFIED,
            target_hardware_addr: EthernetAddress([0; 6]),
            target_protocol_addr: smol_ipv4(GUEST_IP),
        };
        let mut dad_buf = vec![0u8; 14 + dad_repr.buffer_len()];
        let mut deth = EthernetFrame::new_unchecked(&mut dad_buf);
        EthernetRepr {
            src_addr: EthernetAddress(DEFAULT_GUEST_MAC),
            dst_addr: EthernetAddress([0xff; 6]),
            ethertype: EthernetProtocol::Arp,
        }
        .emit(&mut deth);
        dad_repr.emit(&mut ArpPacket::new_unchecked(deth.payload_mut()));
        ch.push_tx_from_guest(dad_buf);
        eng.step(&mut ev, &mut tx, &mut gc, false);
        assert!(ch.pop_rx_for_guest().is_none());

        let mut dhcp = vec![0u8; 244];
        dhcp[0] = 1;
        dhcp[1] = 1;
        dhcp[2] = 6;
        dhcp[4..8].copy_from_slice(&0x1234_5678u32.to_be_bytes());
        dhcp[28..34].copy_from_slice(&DEFAULT_GUEST_MAC);
        dhcp[236..240].copy_from_slice(&DHCP_MAGIC_COOKIE);
        dhcp[240..244].copy_from_slice(&[53, 1, DHCP_DISCOVER, 255]);
        let dhcp_frame = make_udp_eth(
            DEFAULT_GUEST_MAC,
            [0xff; 6],
            Ipv4Addr::UNSPECIFIED,
            68,
            Ipv4Addr::BROADCAST,
            67,
            &dhcp,
        );
        ch.push_tx_from_guest(dhcp_frame);
        eng.step(&mut ev, &mut tx, &mut gc, false);

        let reply = ch.pop_rx_for_guest().expect("expected DHCP OFFER");
        let reth = EthernetFrame::new_checked(&reply).unwrap();
        let rip = Ipv4Packet::new_checked(reth.payload()).unwrap();
        let rudp = UdpPacket::new_checked(rip.payload()).unwrap();
        assert_eq!(rudp.src_port(), 67);
        assert_eq!(rudp.dst_port(), 68);
        assert_eq!(&rudp.payload()[16..20], &GUEST_IP.octets());
        assert_eq!(&rudp.payload()[240..243], &[53, 1, DHCP_OFFER]);
        assert!(!rudp.payload()[240..].windows(2).any(|w| w == [6, 4]));

        let mut nak_req = vec![0u8; 250];
        nak_req[0] = 1;
        nak_req[1] = 1;
        nak_req[2] = 6;
        nak_req[28..34].copy_from_slice(&DEFAULT_GUEST_MAC);
        nak_req[236..240].copy_from_slice(&DHCP_MAGIC_COOKIE);
        nak_req[240..250].copy_from_slice(&[53, 1, DHCP_REQUEST, 50, 4, 192, 168, 64, 5, 255]);
        let (_, _, nak_out) = build_dhcp_reply(&nak_req, false).unwrap();
        assert_eq!(&nak_out[240..243], &[53, 1, DHCP_NAK]);
        assert_eq!(&nak_out[16..20], &[0, 0, 0, 0]);
    }

    #[test]
    fn udp_forward_echo_roundtrip() {
        let ch = NetChannel::new();
        let fwd = parse_forward("udp:127.0.0.1:0:9999").unwrap();
        let mut eng = NetEngine::bind(Arc::clone(&ch), DEFAULT_GUEST_MAC, &[fwd], true).unwrap();
        let host_port = eng.bound_udp_port(0).unwrap();

        let client = UdpSocket::bind("127.0.0.1:0").unwrap();
        client
            .set_read_timeout(Some(Duration::from_secs(2)))
            .unwrap();
        client
            .send_to(b"ping", SocketAddrV4::new(Ipv4Addr::LOCALHOST, host_port))
            .unwrap();

        let mut ev = Vec::new();
        let mut tx = Vec::new();
        let mut gc = StdInstant::now();
        let mut to_guest = None;
        let deadline = StdInstant::now() + Duration::from_secs(1);
        while to_guest.is_none() && StdInstant::now() < deadline {
            eng.step(&mut ev, &mut tx, &mut gc, false);
            to_guest = ch.pop_rx_for_guest();
            if to_guest.is_none() {
                thread::sleep(Duration::from_millis(1));
            }
        }
        let to_guest = to_guest.expect("UDP frame to guest");
        let eth = EthernetFrame::new_checked(&to_guest).unwrap();
        let ip = Ipv4Packet::new_checked(eth.payload()).unwrap();
        let udp = UdpPacket::new_checked(ip.payload()).unwrap();
        assert_eq!(std_ipv4(ip.dst_addr()), GUEST_IP);
        assert_eq!(udp.dst_port(), 9999);
        assert_eq!(udp.payload(), b"ping");

        let reply_frame = make_udp_eth(
            DEFAULT_GUEST_MAC,
            ROUTER_MAC,
            GUEST_IP,
            9999,
            GATEWAY_IP,
            udp.src_port(),
            b": ping",
        );
        ch.push_tx_from_guest(reply_frame);
        eng.step(&mut ev, &mut tx, &mut gc, false);

        let mut out = [0u8; 64];
        let (n, _) = client.recv_from(&mut out).unwrap();
        assert_eq!(&out[..n], b": ping");
    }

    #[test]
    fn outbound_udp_connects_and_drops_spoofed_peer() {
        let ch = NetChannel::new();
        let mut eng = NetEngine::bind(Arc::clone(&ch), DEFAULT_GUEST_MAC, &[], false).unwrap();

        let genuine = UdpSocket::bind("127.0.0.1:0").unwrap();
        let genuine_port = genuine.local_addr().unwrap().port();
        let spoofer = UdpSocket::bind("127.0.0.1:0").unwrap();

        let sock = UdpSocket::bind("127.0.0.1:0").unwrap();
        sock.set_nonblocking(true).unwrap();
        let local_addr = sock.local_addr().unwrap();
        let tok = eng.alloc_udp_token();
        eng.poller.register(sock.as_raw_fd(), tok, EV_READ).unwrap();
        eng.udp_outbound_by_token.insert(
            tok,
            UdpOutboundSession {
                guest_src_ip: GUEST_IP,
                guest_src_port: 55000,
                virtual_dst_ip: DNS_IP,
                virtual_dst_port: 53,
                expected_peer: SocketAddrV4::new(Ipv4Addr::LOCALHOST, genuine_port),
                socket: sock,
                last_active: StdInstant::now(),
            },
        );

        spoofer.send_to(b"spoofed", local_addr).unwrap();
        let mut ev = Vec::new();
        let mut tx = Vec::new();
        let mut gc = StdInstant::now();
        for _ in 0..20 {
            eng.step(&mut ev, &mut tx, &mut gc, false);
            assert!(ch.pop_rx_for_guest().is_none());
            thread::sleep(Duration::from_millis(1));
        }

        genuine.send_to(b"real-dns", local_addr).unwrap();
        let mut got = None;
        let deadline = StdInstant::now() + Duration::from_secs(1);
        while got.is_none() && StdInstant::now() < deadline {
            eng.step(&mut ev, &mut tx, &mut gc, false);
            got = ch.pop_rx_for_guest();
            if got.is_none() {
                thread::sleep(Duration::from_millis(1));
            }
        }
        let frame = got.expect("expected genuine UDP reply");
        let eth = EthernetFrame::new_checked(&frame).unwrap();
        let ip = Ipv4Packet::new_checked(eth.payload()).unwrap();
        let udp = UdpPacket::new_checked(ip.payload()).unwrap();
        assert_eq!(udp.payload(), b"real-dns");
    }

    #[test]
    fn outbound_einprogress_tcp_connect_and_timeout_rst() {
        let ch = NetChannel::new();
        let mut eng = NetEngine::bind(Arc::clone(&ch), DEFAULT_GUEST_MAC, &[], false).unwrap();
        let mut ev = Vec::new();
        let mut tx = Vec::new();
        let mut gc = StdInstant::now();

        let srv = TcpListener::bind("127.0.0.1:0").unwrap();
        let srv_port = srv.local_addr().unwrap().port();
        let srv_thread = thread::spawn(move || {
            let (mut s, _) = srv.accept().unwrap();
            let mut buf = [0u8; 64];
            let n = s.read(&mut buf).unwrap();
            s.write_all(&buf[..n]).unwrap();
        });

        let (stream, _) = connect_nonblocking_tcp(Ipv4Addr::LOCALHOST, srv_port).unwrap();
        let remote_ip = Ipv4Addr::new(93, 184, 216, 34);
        let syn_frame = make_tcp_syn_eth(GUEST_IP, 51234, remote_ip, 80, 1000);
        assert!(eng.register_outbound_tcp(
            GUEST_IP,
            51234,
            remote_ip,
            80,
            stream,
            true,
            1001,
            syn_frame.clone(),
        ));

        ch.push_tx_from_guest(syn_frame);
        eng.step(&mut ev, &mut tx, &mut gc, false);
        assert_eq!(eng.tcp_sessions.len(), 1);

        let mut got_syn_ack = false;
        let deadline = StdInstant::now() + Duration::from_secs(2);
        while !got_syn_ack && StdInstant::now() < deadline {
            eng.step(&mut ev, &mut tx, &mut gc, false);
            while let Some(frame) = ch.pop_rx_for_guest() {
                if let Ok(eth) = EthernetFrame::new_checked(&frame) {
                    if eth.ethertype() == EthernetProtocol::Ipv4 {
                        if let Ok(ip) = Ipv4Packet::new_checked(eth.payload()) {
                            if ip.next_header() == IpProtocol::Tcp {
                                if let Ok(tcp) = TcpPacket::new_checked(ip.payload()) {
                                    assert!(!tcp.rst(), "must not RST during EINPROGRESS");
                                    if tcp.syn() && tcp.ack() {
                                        got_syn_ack = true;
                                    }
                                }
                            }
                        }
                    }
                }
            }
            thread::sleep(Duration::from_millis(1));
        }
        assert!(got_syn_ack, "expected SYN-ACK after EINPROGRESS connect");

        eng.connect_timeout = Duration::from_millis(20);
        let dummy = TcpListener::bind("127.0.0.1:0").unwrap();
        let dummy_port = dummy.local_addr().unwrap().port();
        let (tstream, _) = connect_nonblocking_tcp(Ipv4Addr::LOCALHOST, dummy_port).unwrap();
        let timeout_syn = make_tcp_syn_eth(GUEST_IP, 51235, remote_ip, 443, 2000);
        assert!(eng.register_outbound_tcp(
            GUEST_IP,
            51235,
            remote_ip,
            443,
            tstream,
            true,
            2001,
            timeout_syn,
        ));
        thread::sleep(Duration::from_millis(30));
        eng.pump_tcp_sessions();
        eng.flush_egress_to_guest();
        let mut saw_rst = false;
        while let Some(frame) = ch.pop_rx_for_guest() {
            if let Ok(eth) = EthernetFrame::new_checked(&frame) {
                if let Ok(ip) = Ipv4Packet::new_checked(eth.payload()) {
                    if let Ok(tcp) = TcpPacket::new_checked(ip.payload()) {
                        if tcp.rst() && tcp.dst_port() == 51235 {
                            saw_rst = true;
                        }
                    }
                }
            }
        }
        assert!(saw_rst, "expected RST on connect timeout");
        drop(eng);
        let _ = srv_thread.join();
    }

    #[test]
    fn bad_checksum_syn_does_not_leak_session_and_tokens_tagged() {
        let ch = NetChannel::new();
        let mut eng = NetEngine::bind(Arc::clone(&ch), DEFAULT_GUEST_MAC, &[], false).unwrap();
        let mut bad_syn = make_tcp_syn_eth(GUEST_IP, 52000, Ipv4Addr::new(1, 1, 1, 1), 80, 10);
        let last = bad_syn.len() - 1;
        bad_syn[last] ^= 0xff;
        eng.ingest_guest_frame(bad_syn);
        assert!(eng.tcp_sessions.is_empty());

        eng.next_tcp_seq = 0x1_0005;
        let tok = eng.alloc_tcp_token();
        assert_eq!(tok & TOKEN_KIND_MASK, KIND_TCP_CONN);
        let utok = eng.alloc_udp_token();
        assert_eq!(utok & TOKEN_KIND_MASK, KIND_UDP_OUT);
        assert_ne!(tok, utok);
    }

    #[test]
    fn restrict_denies_outbound_tcp_with_rst_and_tcp_forward_echo_half_close_works() {
        let ch = NetChannel::new();
        let fwd = parse_forward("tcp:127.0.0.1:0:1023").unwrap();
        let mut eng = NetEngine::bind(Arc::clone(&ch), DEFAULT_GUEST_MAC, &[fwd], true).unwrap();
        let host_port = eng.bound_tcp_port(0).unwrap();

        let mut ev = Vec::new();
        let mut tx = Vec::new();
        let mut gc = StdInstant::now();

        let syn_frame = make_tcp_syn_eth(GUEST_IP, 50000, Ipv4Addr::new(1, 1, 1, 1), 80, 100);
        ch.push_tx_from_guest(syn_frame);
        eng.step(&mut ev, &mut tx, &mut gc, false);

        let rst_frame = ch.pop_rx_for_guest().expect("expected RST under --restrict");
        let reth = EthernetFrame::new_checked(&rst_frame).unwrap();
        let rip = Ipv4Packet::new_checked(reth.payload()).unwrap();
        let rtcp = TcpPacket::new_checked(rip.payload()).unwrap();
        assert!(rtcp.rst());
        assert_eq!(rtcp.dst_port(), 50000);

        let mut guest_phy = VirtioPhy::new();
        let mut gcfg =
            IfaceConfig::new(HardwareAddress::Ethernet(EthernetAddress(DEFAULT_GUEST_MAC)));
        gcfg.random_seed = 0x9999;
        let mut guest_iface = Interface::new(gcfg, &mut guest_phy, SmolInstant::from_millis(0));
        guest_iface.update_ip_addrs(|a| {
            let _ = a.push(IpCidr::new(IpAddress::Ipv4(smol_ipv4(GUEST_IP)), 24));
        });
        let _ = guest_iface
            .routes_mut()
            .add_default_ipv4_route(smol_ipv4(GATEWAY_IP));
        let mut guest_socks = SocketSet::new(vec![]);
        let mut srv = TcpSocket::new(
            TcpSocketBuffer::new(vec![0u8; 8192]),
            TcpSocketBuffer::new(vec![0u8; 8192]),
        );
        srv.listen(1023).unwrap();
        let srv_h = guest_socks.add(srv);

        let client_thread = thread::spawn(move || {
            let mut s =
                TcpStream::connect(SocketAddrV4::new(Ipv4Addr::LOCALHOST, host_port)).unwrap();
            s.set_read_timeout(Some(Duration::from_secs(3))).unwrap();
            s.write_all(b"hello-kido-vmm").unwrap();
            s.shutdown(Shutdown::Write).unwrap();
            let mut resp = Vec::new();
            s.read_to_end(&mut resp).unwrap();
            resp
        });

        let start = StdInstant::now();
        let mut guest_buf = Vec::new();
        let mut guest_replied = false;
        while start.elapsed() < Duration::from_secs(3) {
            eng.step(&mut ev, &mut tx, &mut gc, false);
            while let Some(frame) = ch.pop_rx_for_guest() {
                guest_phy.ingress.push_back(frame);
            }
            let now = SmolInstant::from_millis(start.elapsed().as_millis() as i64);
            guest_iface.poll(now, &mut guest_phy, &mut guest_socks);
            {
                let gs = guest_socks.get_mut::<TcpSocket>(srv_h);
                if gs.can_recv() {
                    let mut tmp = [0u8; 256];
                    if let Ok(n) = gs.recv_slice(&mut tmp) {
                        guest_buf.extend_from_slice(&tmp[..n]);
                    }
                }
                if !guest_replied && gs.state() == State::CloseWait && gs.can_send() {
                    let mut out = b"echo:".to_vec();
                    out.extend_from_slice(&guest_buf);
                    gs.send_slice(&out).unwrap();
                    gs.close();
                    guest_replied = true;
                }
            }
            guest_iface.poll(now, &mut guest_phy, &mut guest_socks);
            while let Some(frame) = guest_phy.egress.pop_front() {
                ch.push_tx_from_guest(frame);
            }
            if guest_replied && client_thread.is_finished() {
                break;
            }
            thread::sleep(Duration::from_millis(1));
        }

        let resp = client_thread.join().unwrap();
        assert_eq!(resp, b"echo:hello-kido-vmm");
    }

    #[test]
    fn half_closed_large_host_response_over_128kib_delivers_all_bytes() {
        let ch = NetChannel::new();
        let fwd = parse_forward("tcp:127.0.0.1:0:1023").unwrap();
        let mut eng = NetEngine::bind(Arc::clone(&ch), DEFAULT_GUEST_MAC, &[fwd], true).unwrap();
        let host_port = eng.bound_tcp_port(0).unwrap();

        let payload_len = 192 * 1024usize;
        let expected: Vec<u8> = (0..payload_len)
            .map(|i| ((i.wrapping_mul(31).wrapping_add(17)) & 0xff) as u8)
            .collect();
        let expected_send = expected.clone();

        let mut guest_phy = VirtioPhy::new();
        let mut gcfg =
            IfaceConfig::new(HardwareAddress::Ethernet(EthernetAddress(DEFAULT_GUEST_MAC)));
        gcfg.random_seed = 0x7777;
        let mut guest_iface = Interface::new(gcfg, &mut guest_phy, SmolInstant::from_millis(0));
        guest_iface.update_ip_addrs(|a| {
            let _ = a.push(IpCidr::new(IpAddress::Ipv4(smol_ipv4(GUEST_IP)), 24));
        });
        let _ = guest_iface
            .routes_mut()
            .add_default_ipv4_route(smol_ipv4(GATEWAY_IP));
        let mut guest_socks = SocketSet::new(vec![]);
        let mut srv = TcpSocket::new(
            TcpSocketBuffer::new(vec![0u8; 8192]),
            TcpSocketBuffer::new(vec![0u8; 8192]),
        );
        srv.listen(1023).unwrap();
        let srv_h = guest_socks.add(srv);

        let host_thread = thread::spawn(move || {
            let mut s =
                TcpStream::connect(SocketAddrV4::new(Ipv4Addr::LOCALHOST, host_port)).unwrap();
            s.set_read_timeout(Some(Duration::from_secs(5))).unwrap();
            s.set_write_timeout(Some(Duration::from_secs(5))).unwrap();
            let mut req = Vec::new();
            s.read_to_end(&mut req).unwrap();
            assert_eq!(req, b"GET\n");
            s.write_all(&expected_send).unwrap();
            let _ = s.shutdown(Shutdown::Write);
        });

        let mut ev = Vec::new();
        let mut tx = Vec::new();
        let mut gc = StdInstant::now();
        let start = StdInstant::now();
        let mut sent_req = false;
        let mut got = Vec::with_capacity(payload_len);
        let mut guest_saw_eof = false;

        while start.elapsed() < Duration::from_secs(5) {
            eng.step(&mut ev, &mut tx, &mut gc, false);
            while let Some(frame) = ch.pop_rx_for_guest() {
                guest_phy.ingress.push_back(frame);
            }
            let now = SmolInstant::from_millis(start.elapsed().as_millis() as i64);
            guest_iface.poll(now, &mut guest_phy, &mut guest_socks);
            {
                let gs = guest_socks.get_mut::<TcpSocket>(srv_h);
                if !sent_req && gs.state() == State::Established && gs.can_send() {
                    gs.send_slice(b"GET\n").unwrap();
                    gs.close();
                    sent_req = true;
                }
                while gs.can_recv() {
                    let mut tmp = [0u8; 4096];
                    match gs.recv_slice(&mut tmp) {
                        Ok(0) | Err(_) => break,
                        Ok(n) => got.extend_from_slice(&tmp[..n]),
                    }
                }
                if sent_req && matches!(gs.state(), State::Closed | State::TimeWait) {
                    guest_saw_eof = true;
                }
            }
            guest_iface.poll(now, &mut guest_phy, &mut guest_socks);
            while let Some(frame) = guest_phy.egress.pop_front() {
                ch.push_tx_from_guest(frame);
            }
            if guest_saw_eof && host_thread.is_finished() {
                break;
            }
            thread::sleep(Duration::from_millis(1));
        }

        let host_done = host_thread.is_finished();
        host_thread.join().unwrap();
        let sess_diag: Vec<_> = eng
            .tcp_sessions
            .values()
            .map(|s| {
                let sk = eng.sockets.get::<TcpSocket>(s.handle);
                (sk.state(), sk.send_queue(), s.host_eof, s.guest_eof, s.host.is_some())
            })
            .collect();
        assert!(
            guest_saw_eof,
            "guest did not observe clean host FIN: sent_req={sent_req} got_len={} gs_state={:?} host_done={host_done} eng={sess_diag:?}",
            got.len(),
            guest_socks.get::<TcpSocket>(srv_h).state(),
        );
        assert_eq!(got.len(), expected.len());
        assert_eq!(got, expected);
    }
}
