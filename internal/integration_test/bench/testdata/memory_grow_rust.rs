// Compiled with:
//
//   rustc testdata/memory_grow_rust.rs --edition 2021 \
//     --target wasm32-unknown-unknown --crate-type cdylib -C opt-level=3 \
//     -C panic=abort -C link-arg=--initial-memory=1114112 \
//     -C link-arg=--max-memory=67108864 -C strip=symbols \
//     -o testdata/memory_grow_rust.wasm
//
// Keep every allocation live until the checksum is complete. This prevents the
// allocator from reusing a single block and makes the compiled Rust allocator
// grow linear memory during each call on a fresh module instance.

use std::mem::MaybeUninit;

#[no_mangle]
pub extern "C" fn allocate(chunk_size: usize, chunks: usize) -> u64 {
    let mut allocations = Vec::with_capacity(chunks);
    for chunk in 0..chunks {
        let mut bytes = Vec::<MaybeUninit<u8>>::with_capacity(chunk_size);
        // MaybeUninit keeps the benchmark focused on allocation and memory
        // growth instead of memset. Only the two values read below are set.
        unsafe { bytes.set_len(chunk_size) };
        bytes[0].write(chunk as u8);
        bytes[chunk_size - 1].write((chunk >> 8) as u8);
        allocations.push(bytes);
    }

    allocations
        .iter()
        .map(|bytes| unsafe {
            u64::from(bytes[0].assume_init())
                + u64::from(bytes[bytes.len() - 1].assume_init())
        })
        .sum()
}

// allocate_repeated measures allocator throughput without retaining each
// allocation. The allocator reuses the same block after the first iteration,
// so a large iteration count intentionally dilutes memory.grow overhead.
#[no_mangle]
pub extern "C" fn allocate_repeated(chunk_size: usize, allocations: usize) -> u64 {
    let mut checksum = 0_u64;
    for allocation in 0..allocations {
        let mut bytes = Vec::<MaybeUninit<u8>>::with_capacity(chunk_size);
        unsafe { bytes.set_len(chunk_size) };
        bytes[0].write(allocation as u8);
        bytes[chunk_size - 1].write((allocation >> 8) as u8);
        std::hint::black_box(&mut bytes);
        checksum += unsafe {
            u64::from(bytes[0].assume_init())
                + u64::from(bytes[bytes.len() - 1].assume_init())
        };
    }
    checksum
}
