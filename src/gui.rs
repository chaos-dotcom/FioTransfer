
//! The window, the webview and the JS bridge.
//!
//! The frontend in `frontend/dist` is byte-identical to the Go app's: the same
//! index.html, style.css, app.js and Inter font, rendered by the same WebKit
//! view, in a window with the same size, chrome and background. What changes is
//! only what answers `window.go.main.App[method]`.

#![allow(unsafe_code)] // calls the AppKit/menu shims in mac/

use std::sync::Arc;

use futures::future::BoxFuture;
use serde_json::{json, Value};
use tao::event::{Event, WindowEvent};
use tao::event_loop::{ControlFlow, EventLoopBuilder, EventLoopProxy};
use tao::window::{Window, WindowBuilder};
use tokio::sync::mpsc;
use wry::{DragDropEvent, WebView, WebViewBuilder};

/// The frontend, embedded byte-for-byte from frontend/dist.
pub mod assets {
    pub const INDEX_HTML: &[u8] = include_bytes!("../frontend/dist/index.html");
    pub const STYLE_CSS: &[u8] = include_bytes!("../frontend/dist/style.css");
    pub const APP_JS: &[u8] = include_bytes!("../frontend/dist/app.js");
    pub const INTER: &[u8] = include_bytes!("../frontend/dist/fonts/InterVariable.woff2");
    pub const OFL: &[u8] = include_bytes!("../frontend/dist/fonts/OFL.txt");

    pub fn lookup(path: &str) -> Option<(&'static [u8], &'static str)> {
        let p = path.split(['?', '#']).next().unwrap_or(path);
        match p {
            "/" | "/index.html" => Some((INDEX_HTML, "text/html; charset=utf-8")),
            "/style.css" => Some((STYLE_CSS, "text/css; charset=utf-8")),
            "/app.js" => Some((APP_JS, "text/javascript; charset=utf-8")),
            "/fonts/InterVariable.woff2" => Some((INTER, "font/woff2")),
            "/fonts/OFL.txt" => Some((OFL, "text/plain; charset=utf-8")),
            _ => None,
        }
    }
}

/// The JS the Go app's Wails runtime provided: `window.go.main.App[method]`,
/// `window.runtime.EventsOn`, `window.runtime.ClipboardSetText`.
const BRIDGE_JS: &str = r#"
(function () {
  if (window.__fioBridge) return;
  window.__fioBridge = true;
  var pending = new Map(), seq = 0;
  function invoke(method, args) {
    return new Promise(function (resolve, reject) {
      var id = ++seq;
      pending.set(id, { resolve: resolve, reject: reject });
      window.ipc.postMessage(JSON.stringify({ id: id, method: method, args: args }));
    });
  }
  window.go = { main: { App: new Proxy({}, {
    get: function (_t, name) { return function () { return invoke(String(name), Array.prototype.slice.call(arguments)); }; }
  }) } };
  window.__fioReply = function (id, ok, payload) {
    var p = pending.get(id);
    if (!p) return;
    pending.delete(id);
    if (ok) p.resolve(payload); else p.reject(new Error(payload));
  };
  var handlers = {};
  window.__fioEmit = function (name, payload) {
    (handlers[name] || []).forEach(function (cb) {
      try { cb(payload); } catch (e) { console.error(e); }
    });
  };
  window.runtime = {
    EventsOn: function (name, cb) { (handlers[name] = handlers[name] || []).push(cb); },
    EventsOff: function (name) { delete handlers[name]; },
    EventsEmit: function (name, payload) { window.__fioEmit(name, payload); },
    ClipboardSetText: function (t) { invoke('__clipboard_set', [t]); },
    ClipboardGetText: function () { return invoke('ClipboardText', []); },
    Quit: function () { invoke('__quit', []); },
    WindowShow: function () { invoke('__window_show', []); }
  };
})();
"#;

/// What the app is asked to do, from the frontend or from AppKit.
pub enum UserEvent {
    Emit(String, String),
    Reply(u64, bool, String),
    Menu(i32),
    Show,
    Hide,
    Quit,
    Startup,
}

/// Sends events into the window's webview from any thread.
#[derive(Clone)]
pub struct Emitter {
    proxy: EventLoopProxy<UserEvent>,
}

impl Emitter {
    pub fn emit(&self, event: &str, payload: Value) {
        let _ = self.proxy.send_event(UserEvent::Emit(event.to_string(), payload.to_string()));
    }

    pub fn show(&self) {
        let _ = self.proxy.send_event(UserEvent::Show);
    }

    pub fn hide(&self) {
        let _ = self.proxy.send_event(UserEvent::Hide);
    }

    pub fn quit(&self) {
        let _ = self.proxy.send_event(UserEvent::Quit);
    }

    pub fn menu(&self, tag: i32) {
        let _ = self.proxy.send_event(UserEvent::Menu(tag));
    }
}

/// What the frontend binds to: `App` implements this.
pub trait BridgeHost: Send + Sync + 'static {
    /// Called once the window and webview exist.
    fn startup(&self, emitter: Emitter);
    /// One `window.go.main.App[method](...)` call.
    fn call(&self, method: String, args: Vec<Value>) -> BoxFuture<'static, Result<Value, String>>;

    /// A menu item was chosen (the File menu, or the menu bar item).
    fn menu(&self, tag: i32);

    /// Whether the window may close to the menu bar (false asks first).
    fn before_close(&self) -> bool {
        true
    }
}

pub struct WindowOptions {
    pub start_hidden: bool,
}

extern "C" {
    fn fioMenuInit(cb: extern "C" fn(i32));
    fn fioWindowStyle();
    fn fioSnapshotWebView(path: *const std::os::raw::c_char) -> i32;
    fn fioEvalJS(js: *const std::os::raw::c_char);
    fn fioEvalJSFile(js: *const std::os::raw::c_char, path: *const std::os::raw::c_char) -> i32;
}

/// The webview's own pixels, for the measuring harness. Must not be called on
/// the main thread (it waits for the main queue).
pub fn snapshot_webview(path: &str) -> bool {
    let c = std::ffi::CString::new(path).unwrap_or_default();
    unsafe { fioSnapshotWebView(c.as_ptr()) == 1 }
}

pub fn eval_js(js: &str) {
    let c = std::ffi::CString::new(js).unwrap_or_default();
    unsafe { fioEvalJS(c.as_ptr()) }
}

pub fn eval_js_to_file(js: &str, path: &str) -> bool {
    let j = std::ffi::CString::new(js).unwrap_or_default();
    let p = std::ffi::CString::new(path).unwrap_or_default();
    unsafe { fioEvalJSFile(j.as_ptr(), p.as_ptr()) == 1 }
}

extern "C" fn menu_callback(tag: i32) {
    MENU_PROXY.with(|p| {
        if let Some(proxy) = p.borrow().as_ref() {
            let _ = proxy.send_event(UserEvent::Menu(tag));
        }
    });
}

thread_local! {
    static MENU_PROXY: std::cell::RefCell<Option<EventLoopProxy<UserEvent>>> = const { std::cell::RefCell::new(None) };
}

/// Serves the embedded frontend on 127.0.0.1, so the webview loads the real
/// pages with the real relative URLs (and nothing changes in the assets).
pub fn serve_assets(listener: std::net::TcpListener) -> u16 {
    let port = listener.local_addr().map(|a| a.port()).unwrap_or(0);
    std::thread::Builder::new()
        .name("fio-assets".into())
        .spawn(move || {
            listener.set_nonblocking(true).ok();
            let rt = match tokio::runtime::Builder::new_current_thread().enable_all().build() {
                Ok(rt) => rt,
                Err(_) => return,
            };
            rt.block_on(async move {
                let Ok(l) = tokio::net::TcpListener::from_std(listener) else { return };
                loop {
                    let Ok((mut sock, _)) = l.accept().await else { continue };
                    tokio::spawn(async move {
                        use tokio::io::{AsyncReadExt, AsyncWriteExt};
                        let mut buf = vec![0u8; 8192];
                        let mut read = 0;
                        loop {
                            match sock.read(&mut buf[read..]).await {
                                Ok(0) => break,
                                Ok(n) => {
                                    read += n;
                                    if buf[..read].windows(4).any(|w| w == b"\r\n\r\n") || read == buf.len() {
                                        break;
                                    }
                                }
                                Err(_) => return,
                            }
                        }
                        let req = String::from_utf8_lossy(&buf[..read]).into_owned();
                        let path = req.split_whitespace().nth(1).unwrap_or("/").to_string();
                        let (body, ctype) = match assets::lookup(&path) {
                            Some((b, c)) => (b, c),
                            None => (&b"not found"[..], "text/plain; charset=utf-8"),
                        };
                        let head = format!(
                            "HTTP/1.1 200 OK\r\nContent-Type: {}\r\nContent-Length: {}\r\nCache-Control: no-store\r\nConnection: close\r\n\r\n",
                            ctype,
                            body.len()
                        );
                        let _ = sock.write_all(head.as_bytes()).await;
                        let _ = sock.write_all(body).await;
                        let _ = sock.flush().await;
                    });
                }
            });
        })
        .ok();
    port
}

/// Creates the window, the webview and the menus, then runs the event loop.
pub fn run(host: Arc<dyn BridgeHost>, opts: WindowOptions) -> i32 {
    let event_loop = EventLoopBuilder::<UserEvent>::with_user_event().build();
    let proxy = event_loop.create_proxy();
    MENU_PROXY.with(|p| *p.borrow_mut() = Some(proxy.clone()));

    let window: Window = match WindowBuilder::new()
        .with_title("FioTransfer")
        .with_inner_size(tao::dpi::LogicalSize::new(1180.0, 780.0))
        .with_min_inner_size(tao::dpi::LogicalSize::new(860.0, 560.0))
        .with_visible(false)
        .build(&event_loop)
    {
        Ok(w) => w,
        Err(e) => {
            eprintln!("FioTransfer: could not open a window: {}", e);
            return 1;
        }
    };

    let listener = std::net::TcpListener::bind("127.0.0.1:0").expect("bind asset server");
    let port = serve_assets(listener);
    let url = format!("http://127.0.0.1:{}/index.html", port);

    // The menu is built before the run loop starts, as Wails did.
    unsafe { fioMenuInit(menu_callback) };

    let (tx, _rx) = mpsc::unbounded_channel::<(u64, String, Vec<Value>)>();
    let host_for_ipc = host.clone();
    let proxy_for_ipc = proxy.clone();
    let proxy_for_drop = proxy.clone();
    let rt = tokio::runtime::Handle::current();

    let webview: WebView = match WebViewBuilder::new()
        .with_url(url)
        .with_initialization_script(BRIDGE_JS)
        .with_drag_drop_handler(move |e: DragDropEvent| {
            if let DragDropEvent::Drop { paths, .. } = e {
                let list: Vec<String> = paths.iter().map(|p| p.to_string_lossy().into_owned()).collect();
                let _ = proxy_for_drop.send_event(UserEvent::Emit("files-dropped".into(), json!(list).to_string()));
            }
            false
        })
        .with_ipc_handler(move |req: wry::http::Request<String>| {
            let body = req.body().clone();
            let tx = tx.clone();
            let host = host_for_ipc.clone();
            let proxy = proxy_for_ipc.clone();
            rt.spawn(async move {
                let parsed: Value = match serde_json::from_str(&body) {
                    Ok(v) => v,
                    Err(_) => return,
                };
                let id = parsed.get("id").and_then(|v| v.as_u64()).unwrap_or(0);
                let method = parsed.get("method").and_then(|v| v.as_str()).unwrap_or("").to_string();
                let args: Vec<Value> = parsed
                    .get("args")
                    .and_then(|v| v.as_array())
                    .cloned()
                    .unwrap_or_default();
                let fut = host.call(method, args);
                let res = fut.await;
                let (ok, payload) = match res {
                    Ok(v) => (true, v.to_string()),
                    Err(e) => (false, Value::String(e).to_string()),
                };
                let _ = proxy.send_event(UserEvent::Reply(id, ok, payload));
            });
        })
        .build(&window)
    {
        Ok(w) => w,
        Err(e) => {
            eprintln!("FioTransfer: could not create the webview: {}", e);
            return 1;
        }
    };

    unsafe { fioWindowStyle() };

    let mut started = false;
    let mut hidden = opts.start_hidden;
    let webview_ref: Arc<WebView> = Arc::new(webview);
    let wv = webview_ref.clone();

    event_loop.run(move |event, _target, control_flow| {
        *control_flow = ControlFlow::Wait;
        match event {
            Event::NewEvents(tao::event::StartCause::Init) => {
                if !started {
                    started = true;
                    host.startup(Emitter { proxy: proxy.clone() });
                }
                if !hidden {
                    window.set_visible(true);
                    window.set_focus();
                }
            }
            Event::UserEvent(UserEvent::Emit(name, payload)) => {
                let js = format!(
                    "window.__fioEmit({}, {});",
                    serde_json::to_string(&name).unwrap_or_else(|_| "\"\"".into()),
                    payload
                );
                let _ = wv.evaluate_script(&js);
            }
            Event::UserEvent(UserEvent::Reply(id, ok, payload)) => {
                let js = format!("window.__fioReply({}, {}, {});", id, ok, payload);
                let _ = wv.evaluate_script(&js);
            }
            Event::UserEvent(UserEvent::Show) => {
                hidden = false;
                window.set_visible(true);
                window.set_minimized(false);
                window.set_focus();
            }
            Event::UserEvent(UserEvent::Hide) => {
                hidden = true;
                window.set_visible(false);
            }
            Event::UserEvent(UserEvent::Quit) => {
                *control_flow = ControlFlow::Exit;
            }
            Event::UserEvent(UserEvent::Menu(tag)) => host.menu(tag),
            Event::WindowEvent { event: WindowEvent::CloseRequested, .. } => {
                // The window closes to the menu bar, as in the Go app.
                if host.before_close() {
                    window.set_visible(false);
                    hidden = true;
                }
            }
            _ => {}
        }
    });
}
