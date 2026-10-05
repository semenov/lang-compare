#![allow(dangerous_implicit_autorefs)]
mod compiler;
mod memory;
mod object;
mod scanner;
mod value;
mod vm;

use std::process::exit;

#[global_allocator]
static GLOBAL: mimalloc::MiMalloc = mimalloc::MiMalloc;

fn main() {
    let args: Vec<String> = std::env::args().collect();
    if args.len() != 2 {
        eprintln!("Usage: lox [path]");
        exit(64);
    }
    let source = match std::fs::read(&args[1]) {
        Ok(b) => String::from_utf8_lossy(&b).into_owned(),
        Err(_) => {
            eprintln!("Could not open file \"{}\".", args[1]);
            exit(74);
        }
    };
    let mut vm = vm::Vm::new();
    let code = match vm.interpret(&source) {
        vm::InterpretResult::Ok => 0,
        vm::InterpretResult::CompileError => 65,
        vm::InterpretResult::RuntimeError => 70,
    };
    // Skip tearing down the heap; the OS reclaims it.
    std::mem::forget(vm);
    exit(code);
}
