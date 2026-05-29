---
name: __PROJECT_NAME_LOWER__-mvc-appkit
description: __PROJECT_NAME__'s architectural contract — every feature lives in an NSViewController that owns its model, its view, AND its HTTP test-API routes. Use this skill on every code-writing turn for the __PROJECT_NAME__ project.
---

# __PROJECT_NAME__ MVC for macOS / AppKit

This skill is **mandatory** for every code change to the __PROJECT_NAME__
project. It defines how every feature is structured so that:

1. Business logic, UI, and orchestration are cleanly separated.
2. Every controller can be driven from the localhost HTTP test API
   without spawning a window or simulating input.
3. The feature-test harness in 007-builder probes features through
   a single uniform shape: `/<controller>/<action>`.

If your slice does not fit this shape, the slice is mis-spec'd —
write `state.json` with `{"action":"abort","reason":"non-mvc-slice"}`
and exit. Do not improvise.

---

## The three roles

### Model

- Plain Swift structs / classes in `__PROJECT_NAME__/<Feature>/<Name>State.swift`.
- Holds data and business rules. No `import AppKit`. No `NSView`,
  no `NSWindow`, no `NSTextStorage` here.
- Examples: `DocumentState` (text + dirty + URL + encoding),
  `FindState` (pattern + match-case + wrap), `ZoomLevel`.

### View

- An `NSView` subclass in `__PROJECT_NAME__/<Feature>/<Name>View.swift`.
- Renders the model. Captures user gestures. Forwards them via
  delegate callbacks or `NSResponder` actions.
- Must not call AppKit panels (`NSOpenPanel`, `NSAlert`, …).
  Must not touch the test API. Must not own state that outlives
  itself.
- Constructed via the canonical NSTextView chain when text is
  involved (PRD §8.2).

### Controller

- An `NSViewController` subclass in
  `__PROJECT_NAME__/<Feature>/<Name>Controller.swift`.
- Owns one Model instance and one View instance.
- **Registers its HTTP routes** with the `TestAPIRouter` at
  `viewDidLoad`. This is the load-bearing part of the pattern.

---

## The controller-owns-route rule

This is the single most important contract in __PROJECT_NAME__. Every
controller exposes a namespaced route prefix and registers every
endpoint that controller is responsible for.

### Required pattern

```swift
import AppKit

final class EditorController: NSViewController {

    let state = EditorState()
    let editorView: EditorScrollView

    init() {
        self.editorView = EditorScrollView(frame: .zero)
        super.init(nibName: nil, bundle: nil)
    }
    required init?(coder: NSCoder) { fatalError() }

    override func loadView() {
        view = editorView
    }

    override func viewDidLoad() {
        super.viewDidLoad()
        // Wire the model → view binding here.
        editorView.editor.string = state.text
        editorView.editor.delegate = self

        // REQUIRED: register routes for this controller's actions.
        TestAPIRouter.shared.register(controller: self)
    }
}

// Routes live in an extension next to the controller, NEVER in a
// separate file under TestAPI/. The controller owns its own API.
extension EditorController: TestAPIControllerRoutes {

    static var routePrefix: String { "editor" }

    func registerRoutes(on router: TestAPIRouter) {
        router.get(prefix: Self.routePrefix, path: "/text") { [weak self] _ in
            guard let self else { return .notFound }
            let body = try? JSONEncoder().encode(["text": self.state.text])
            return .ok(json: body ?? Data())
        }
        router.post(prefix: Self.routePrefix, path: "/type") { [weak self] req in
            guard let self else { return .notFound }
            struct Body: Decodable { let text: String }
            guard let b = try? JSONDecoder().decode(Body.self, from: req.body) else {
                return .badRequest("body must be {\"text\": String}")
            }
            DispatchQueue.main.sync { self.appendText(b.text) }
            return .ok(json: Data(#"{"ok":true}"#.utf8))
        }
    }

    private func appendText(_ s: String) {
        // model mutation + view binding; no panel, no I/O, no recursion
        state.text.append(s)
        editorView.editor.string = state.text
        state.isDirty = true
    }
}
```

### The router

The shared `TestAPIRouter` is a flat registry — controllers
register themselves into it, but each route ALWAYS sits in
`<routePrefix>/<path>`. The router never invents routes; it only
delegates.

```swift
final class TestAPIRouter {
    static let shared = TestAPIRouter()
    private var handlers: [String: (TestAPIRequest) -> TestAPIResponse] = [:]

    func register<C: TestAPIControllerRoutes>(controller: C) {
        controller.registerRoutes(on: self)
    }
    func get(prefix: String, path: String, _ h: @escaping (TestAPIRequest) -> TestAPIResponse) {
        handlers["GET /\(prefix)\(path)"] = h
    }
    func post(prefix: String, path: String, _ h: @escaping (TestAPIRequest) -> TestAPIResponse) {
        handlers["POST /\(prefix)\(path)"] = h
    }
    func dispatch(_ req: TestAPIRequest) -> TestAPIResponse {
        handlers["\(req.method) \(req.path)"] ?? .notFound(req)
    }
}

protocol TestAPIControllerRoutes {
    static var routePrefix: String { get }
    func registerRoutes(on router: TestAPIRouter)
}
```

### Required endpoints per controller

| Controller         | Prefix      | Min endpoints (PRD §7.3)                                  |
|--------------------|-------------|-----------------------------------------------------------|
| AppController      | `/app`      | `GET /healthz`, `POST /shutdown`                          |
| WindowController   | `/window`   | `GET /list`, `GET /screenshot`                            |
| EditorController   | `/editor`   | `GET /text`, `POST /type`, `GET /state`                   |
| DocumentController | `/document` | `POST /openFile`, `POST /saveAs`                          |
| MenuController     | `/menu`     | `POST /invoke {path:[...]}`                               |
| ShortcutController | `/shortcut` | `POST /press {keys:"cmd+s"}`                              |

A new feature MUST either:
- belong to an existing controller (add a route under the existing
  prefix), OR
- introduce a new controller with its own prefix + route file.

Never add a top-level route. The flat `/healthz` is the only one
allowed (it lives on `AppController` with no prefix).

---

## Project structure (canonical)

```
__PROJECT_NAME__/
├── main.swift                              # NSApplication.shared.run()
├── AppDelegate.swift                       # instantiates AppController
│
├── App/
│   ├── AppController.swift                 # /app/healthz, /app/shutdown
│   └── TestAPI/
│       ├── TestAPIServer.swift             # HTTP listener
│       ├── TestAPIRouter.swift             # flat registry
│       └── TestAPIRequest+Response.swift   # value types
│
├── Window/
│   ├── WindowController.swift              # /window/list, /window/screenshot
│   ├── WindowState.swift                   # model
│   └── __PROJECT_NAME__Window.swift                   # NSWindow subclass
│
├── Editor/
│   ├── EditorController.swift              # /editor/*  (routes live here!)
│   ├── EditorState.swift                   # model
│   ├── EditorView.swift                    # NSTextView subclass
│   └── EditorScrollView.swift              # NSScrollView with canonical chain
│
├── Document/
│   ├── DocumentController.swift            # /document/*
│   ├── DocumentState.swift
│   └── DocumentReader.swift                # encoding + EOL detect
│
├── Menu/
│   └── MenuController.swift                # /menu/invoke
│
└── Theme/                                  # Colors, Fonts, Metrics (no controllers)
```

**Routes never live in their own file under `TestAPI/`.**
That was the old design and produced sprawling sidebar-of-routes
files. In the new design, finding where `POST /editor/type` is
handled is one `grep -r "/type"` away — but actually it's the
EditorController file by name, no grep needed.

---

## What this skill rejects

The orchestrator's quality check will block the PR on any of:

1. A route registered outside its controller. (e.g. `POST /editor/type`
   declared in `__PROJECT_NAME__/App/TestAPI/Routes.swift`.)
2. An `NSView` subclass that imports `Foundation.URLSession`,
   `TestAPIRouter`, or any TestAPI symbol. Views never speak API.
3. A controller without a `routePrefix` if it has any user-visible
   behavior.
4. A new top-level route (no controller prefix). Only `/healthz` is
   exempt, and even that is on `AppController` for historical
   reasons.
5. A `MainViewController` / `HomeViewController` / catch-all
   controller. One controller per *coherent feature*; if it grows
   past ~200 lines, decompose.
6. Model code that imports AppKit.
7. View code that holds `var state` that outlives the view's
   lifetime. Long-lived state belongs to the controller.

---

## Why this pattern (the rationale)

The previous notepad/ project taught us that the unit of testable
behavior is the controller, not the window. The reason is purely
operational: 007-builder's feature check fires HTTP probes; if a
behavior is reachable only by clicking the view, it's not testable
at all.

By forcing every controller to declare its own routes:

- The "what does this feature actually do?" question is answered in
  one file.
- The probe path is predictable (`/<prefix>/<action>`), so the
  planner can write acceptance JSON without reading code.
- Adding a feature means adding a controller, not weaving logic
  into a god-object.
- Code review can grep for controllers without routes and reject
  them automatically (the thermo-nuclear skill does exactly this).

---

## Workflow on each code-writing turn

1. Read the issue body. Identify which controller it touches
   (new or existing).
2. Open that controller file. If it doesn't exist, create it
   with the canonical scaffolding above.
3. Add the model field, the view binding, and the route registration
   *in the same file*.
4. If the acceptance probes name a new endpoint, the corresponding
   route handler exists in the SAME commit.
5. Build. Run feature test. Commit.

One slice = one controller + its routes + its model fields.
Resist the temptation to refactor adjacent controllers.
