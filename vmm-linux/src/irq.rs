use std::sync::{Arc, Mutex};

pub const MAX_SPIS: usize = 96;

pub trait SpiBackend: Send + Sync {
    fn set_spi_level(&self, spi: u32, active: bool);
}

pub struct SpiMux {
    backend: Arc<dyn SpiBackend>,
    sources: Mutex<[u64; MAX_SPIS]>,
}

impl SpiMux {
    pub fn new(backend: Arc<dyn SpiBackend>) -> Arc<Self> {
        Arc::new(Self {
            backend,
            sources: Mutex::new([0u64; MAX_SPIS]),
        })
    }

    pub fn set_source(&self, spi: u32, source_id: u32, asserted: bool) {
        let idx = spi as usize;
        if idx >= MAX_SPIS || source_id >= 64 {
            return;
        }
        let bit = 1u64 << source_id;
        let mut guard = self.sources.lock().unwrap();
        let old_mask = guard[idx];
        let new_mask = if asserted {
            old_mask | bit
        } else {
            old_mask & !bit
        };
        if old_mask == new_mask {
            return;
        }
        guard[idx] = new_mask;
        let old_level = old_mask != 0;
        let new_level = new_mask != 0;
        if old_level != new_level {
            self.backend.set_spi_level(spi, new_level);
        }
    }

    pub fn line(self: &Arc<Self>, spi: u32, source_id: u32) -> IrqLine {
        IrqLine {
            mux: Arc::clone(self),
            spi,
            source_id,
        }
    }
}

#[derive(Clone)]
pub struct IrqLine {
    mux: Arc<SpiMux>,
    spi: u32,
    source_id: u32,
}

impl IrqLine {
    #[inline]
    pub fn set_level(&self, asserted: bool) {
        self.mux.set_source(self.spi, self.source_id, asserted);
    }
}

#[cfg(test)]
pub mod test_util {
    use super::*;

    pub struct MockSpiBackend {
        pub events: Mutex<Vec<(u32, bool)>>,
        pub levels: Mutex<[bool; MAX_SPIS]>,
    }

    impl MockSpiBackend {
        pub fn new() -> Arc<Self> {
            Arc::new(Self {
                events: Mutex::new(Vec::new()),
                levels: Mutex::new([false; MAX_SPIS]),
            })
        }

        pub fn level(&self, spi: u32) -> bool {
            self.levels.lock().unwrap()[spi as usize]
        }

        pub fn events(&self) -> Vec<(u32, bool)> {
            self.events.lock().unwrap().clone()
        }
    }

    impl SpiBackend for MockSpiBackend {
        fn set_spi_level(&self, spi: u32, active: bool) {
            self.levels.lock().unwrap()[spi as usize] = active;
            self.events.lock().unwrap().push((spi, active));
        }
    }

    #[test]
    fn shared_spi_logical_or_refcount() {
        let backend = MockSpiBackend::new();
        let mux = SpiMux::new(backend.clone());
        let l0 = mux.line(4, 0);
        let l1 = mux.line(4, 1);

        l0.set_level(true);
        assert!(backend.level(4));
        l1.set_level(true);
        assert!(backend.level(4));
        // Only one rising transition
        assert_eq!(backend.events(), vec![(4, true)]);

        l0.set_level(false);
        assert!(backend.level(4));
        assert_eq!(backend.events(), vec![(4, true)]);

        l1.set_level(false);
        assert!(!backend.level(4));
        assert_eq!(backend.events(), vec![(4, true), (4, false)]);
    }
}
