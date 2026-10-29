
use std::path::Path;

fn main() {
    println!("cargo:rerun-if-changed=mac");
    let out = std::env::var("OUT_DIR").unwrap();
    let mut build = cc::Build::new();
    build
        .files([
            "mac/tray_darwin.m",
            "mac/quick_darwin.m",
            "mac/services_darwin.m",
            "mac/dialogs.m",
            "mac/window.m",
        ])
        .flag("-fobjc-arc")
        .flag("-Wno-deprecated-declarations")
        .flag("-mmacosx-version-min=11.0")
        .include("mac")
        .warnings(false);
    build.compile("fio_mac");
    for fw in ["Cocoa", "WebKit", "QuartzCore"] {
        println!("cargo:rustc-link-lib=framework={fw}");
    }
    println!("cargo:rustc-link-arg=-Wl,-rpath,@executable_path/../Frameworks");
    let _ = Path::new(&out);
}
