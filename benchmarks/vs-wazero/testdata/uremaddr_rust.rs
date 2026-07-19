#![no_std]

use core::panic::PanicInfo;

#[panic_handler]
fn panic(_: &PanicInfo) -> ! {
    loop {}
}

#[no_mangle]
pub static mut BUFFER: [u8; 8191] = [1; 8191];

#[no_mangle]
pub unsafe extern "C" fn sum_mod(indices: *const u32, len: u32) -> u32 {
    let buffer = core::ptr::addr_of!(BUFFER).cast::<u8>();
    let mut sum = 0u32;
    let mut i = 0u32;
    while i < len {
        let index = core::ptr::read_volatile(indices.add(i as usize));
        let value = core::ptr::read_volatile(buffer.add((index % 8191) as usize));
        sum = sum.wrapping_add(value as u32);
        i += 1;
    }
    sum
}
