#!/usr/bin/env node
"use strict";
/**
 * openclaw-proxy: Bridge OpenClaw TypeScript plugins to Fluctio JSON-RPC protocol.
 *
 * Usage:
 *   node proxy.js <path-to-openclaw-plugin>
 *
 * The proxy loads an OpenClaw plugin, captures its register() calls,
 * and exposes the registered capabilities via Fluctio's JSON-RPC stdin/stdout protocol.
 */
var __createBinding = (this && this.__createBinding) || (Object.create ? (function(o, m, k, k2) {
    if (k2 === undefined) k2 = k;
    var desc = Object.getOwnPropertyDescriptor(m, k);
    if (!desc || ("get" in desc ? !m.__esModule : desc.writable || desc.configurable)) {
      desc = { enumerable: true, get: function() { return m[k]; } };
    }
    Object.defineProperty(o, k2, desc);
}) : (function(o, m, k, k2) {
    if (k2 === undefined) k2 = k;
    o[k2] = m[k];
}));
var __setModuleDefault = (this && this.__setModuleDefault) || (Object.create ? (function(o, v) {
    Object.defineProperty(o, "default", { enumerable: true, value: v });
}) : function(o, v) {
    o["default"] = v;
});
var __importStar = (this && this.__importStar) || (function () {
    var ownKeys = function(o) {
        ownKeys = Object.getOwnPropertyNames || function (o) {
            var ar = [];
            for (var k in o) if (Object.prototype.hasOwnProperty.call(o, k)) ar[ar.length] = k;
            return ar;
        };
        return ownKeys(o);
    };
    return function (mod) {
        if (mod && mod.__esModule) return mod;
        var result = {};
        if (mod != null) for (var k = ownKeys(mod), i = 0; i < k.length; i++) if (k[i] !== "default") __createBinding(result, mod, k[i]);
        __setModuleDefault(result, mod);
        return result;
    };
})();
Object.defineProperty(exports, "__esModule", { value: true });
const readline_1 = require("readline");
const path = __importStar(require("path"));
const url_1 = require("url");
// ─── Captured capabilities ──────────────────────────────────────────────────
const registeredTools = new Map();
const registeredChannels = new Map();
let pluginConfig = {};
// ─── Mock OpenClaw Registration API ─────────────────────────────────────────
function createMockApi() {
    return {
        registerTool(def) {
            const tool = {
                name: def.name || def.id,
                description: def.description || "",
                parameters: def.parameters || def.inputSchema,
                execute: def.execute || def.run || (async () => "not implemented"),
            };
            registeredTools.set(tool.name, tool);
            log(`registered tool: ${tool.name}`);
        },
        registerChannel(def) {
            const channel = {
                id: def.id || def.name,
                name: def.name || def.id,
                send: def.send || def.sendMessage || (async () => { }),
                start: def.start || def.connect,
            };
            registeredChannels.set(channel.id, channel);
            log(`registered channel: ${channel.id}`);
        },
        // Capture but don't bridge these yet
        registerProvider(def) {
            log(`registered provider: ${def.id || def.name} (not bridged)`);
        },
        registerSpeechProvider(def) {
            log(`registered speech provider: ${def.id || def.name} (not bridged)`);
        },
        registerImageGenerationProvider(def) {
            log(`registered image gen provider: ${def.id || def.name} (not bridged)`);
        },
        registerMediaUnderstandingProvider(def) {
            log(`registered media provider: ${def.id || def.name} (not bridged)`);
        },
        registerWebSearchProvider(def) {
            log(`registered web search provider: ${def.id || def.name} (not bridged)`);
        },
        registerHttpRoute(def) {
            log(`registered http route: ${def.path || "unknown"} (not bridged)`);
        },
        registerHook: () => { },
        on: () => { },
        onConversationBindingResolved: () => { },
    };
}
// ─── JSON-RPC I/O ───────────────────────────────────────────────────────────
function send(obj) {
    process.stdout.write(JSON.stringify(obj) + "\n");
}
function sendResult(id, result) {
    send({ jsonrpc: "2.0", id, result });
}
function sendError(id, code, message) {
    send({ jsonrpc: "2.0", id, error: { code, message } });
}
function sendNotification(method, params) {
    send({ jsonrpc: "2.0", method, params });
}
function log(msg) {
    process.stderr.write(`[openclaw-proxy] ${msg}\n`);
}
// ─── Request handlers ───────────────────────────────────────────────────────
async function handleRequest(req) {
    try {
        switch (req.method) {
            case "initialize":
                pluginConfig = req.params?.config || {};
                // Start channel plugins if any
                for (const [id, ch] of registeredChannels) {
                    if (ch.start) {
                        ch.start({
                            onMessage: (msg) => {
                                sendNotification("message.inbound", {
                                    channel: msg.channel || `plugin:openclaw-proxy`,
                                    chatId: msg.chatId,
                                    userId: msg.userId,
                                    text: msg.text,
                                    peerKind: msg.peerKind || "dm",
                                    senderName: msg.senderName || "",
                                });
                            },
                        }).catch((err) => log(`channel ${id} start error: ${err.message}`));
                    }
                }
                sendResult(req.id, { status: "ok" });
                break;
            case "shutdown":
                sendResult(req.id, { status: "ok" });
                setTimeout(() => process.exit(0), 100);
                break;
            case "tool.list":
                const tools = Array.from(registeredTools.values()).map((t) => ({
                    name: t.name,
                    description: t.description,
                    parameters: t.parameters,
                }));
                sendResult(req.id, { tools });
                break;
            case "tool.execute": {
                const toolName = req.params?.name;
                const args = req.params?.args || {};
                const tool = registeredTools.get(toolName);
                if (!tool) {
                    sendError(req.id, -32602, `tool not found: ${toolName}`);
                    return;
                }
                // OpenClaw tools have two signatures:
                //   1. execute(args)                         — simple (arity 1)
                //   2. execute(toolCallId, params, signal)   — OpenClaw standard (arity 2+)
                // Dispatch by DECLARED ARITY, not by try/catch probing: the old
                // probe called every tool as execute(id, args) first, so a
                // simple-signature tool that doesn't throw (most don't) silently
                // received the toolCallId string as its args and lost the real
                // parameters. Arity is reliable for normally-declared functions;
                // arity 0 (rest args / arguments-object styles) is genuinely
                // ambiguous and keeps a probe fallback, logged per call so the
                // double execution is diagnosable.
                const arity = typeof tool.execute === "function" ? tool.execute.length : 0;
                let rawResult;
                if (arity === 1) {
                    rawResult = await tool.execute(args);
                }
                else if (arity >= 2) {
                    rawResult = await tool.execute(String(req.id), args);
                }
                else {
                    log(`tool ${toolName}: declared arity 0 — signature shape unknown, probing standard-shape call first`);
                    try {
                        rawResult = await tool.execute(String(req.id), args);
                    }
                    catch (firstErr) {
                        log(`tool ${toolName}: standard-signature call threw (${firstErr?.message || "unknown"}) — retrying as simple execute(args)`);
                        rawResult = await tool.execute(args);
                    }
                }
                // Normalize result — OpenClaw tools may return { content: [{ text }] }
                let result;
                if (typeof rawResult === "string") {
                    result = rawResult;
                }
                else if (rawResult?.content?.[0]?.text) {
                    result = rawResult.content.map((c) => c.text).join("\n");
                }
                else {
                    result = JSON.stringify(rawResult);
                }
                sendResult(req.id, { result });
                break;
            }
            case "channel.send": {
                const chatId = req.params?.chatId;
                const text = req.params?.text;
                // Send to first available channel
                const ch = registeredChannels.values().next().value;
                if (!ch) {
                    sendError(req.id, -32602, "no channel registered");
                    return;
                }
                await ch.send({ chatId, text });
                sendResult(req.id, { ok: true });
                break;
            }
            default:
                sendError(req.id, -32601, `method not found: ${req.method}`);
        }
    }
    catch (err) {
        sendError(req.id, -32000, err.message || "internal error");
    }
}
// ─── Main ───────────────────────────────────────────────────────────────────
// dynamicImport loads a module as TRUE ESM regardless of this file's own
// module format. tsc's commonjs downcompile rewrites a literal
// `await import(...)` into a require()-based shim (that's how the old
// proxy.js ended up with a fake "Try as ESM" branch), so the call goes
// through eval to survive into the emitted JS as a real dynamic import.
// The specifier is a file URL: bare Windows paths (C:\…) are not valid
// ESM specifiers.
function dynamicImport(specifier) {
    return eval(`import(${JSON.stringify((0, url_1.pathToFileURL)(specifier).href)})`);
}
async function main() {
    const pluginPath = process.argv[2];
    if (!pluginPath) {
        process.stderr.write("Usage: openclaw-proxy <path-to-openclaw-plugin>\n");
        process.exit(1);
    }
    // Resolve plugin path
    const resolved = path.resolve(pluginPath);
    log(`loading OpenClaw plugin from: ${resolved}`);
    // Load the plugin module
    let pluginModule;
    try {
        pluginModule = require(resolved);
    }
    catch (err) {
        // Genuine ESM fallback. dynamicImport (below) keeps a real `import()`
        // in the compiled proxy.js — a bare `await import(...)` here gets
        // downcompiled by tsc (module: commonjs) into a require() shim, which
        // fails for .mjs / "type": "module" plugins exactly like the first
        // require did. Both failure reasons are reported so an operator can
        // tell a broken CJS plugin from a broken ESM one.
        try {
            pluginModule = await dynamicImport(resolved);
        }
        catch (esmErr) {
            process.stderr.write(`Failed to load plugin — require: ${err.message}; import: ${esmErr.message}\n`);
            process.exit(1);
        }
    }
    // Get the plugin definition
    const plugin = pluginModule.default || pluginModule;
    // Create mock API and run register()
    const api = createMockApi();
    if (typeof plugin === "function") {
        // definePluginEntry style — function that returns definition
        const def = plugin();
        if (def && typeof def.register === "function") {
            def.register(api);
        }
    }
    else if (plugin && typeof plugin.register === "function") {
        // Direct export style
        plugin.register(api);
    }
    else {
        log("warning: plugin has no register() method, checking for direct exports");
        // Try to find tools/channels exported directly
        if (plugin.tools) {
            for (const t of Array.isArray(plugin.tools) ? plugin.tools : [plugin.tools]) {
                api.registerTool(t);
            }
        }
        if (plugin.channel) {
            api.registerChannel(plugin.channel);
        }
    }
    log(`loaded: ${registeredTools.size} tools, ${registeredChannels.size} channels`);
    // Start JSON-RPC loop
    const rl = (0, readline_1.createInterface)({ input: process.stdin, crlfDelay: Infinity });
    rl.on("line", (line) => {
        const trimmed = line.trim();
        if (!trimmed)
            return;
        let req;
        try {
            req = JSON.parse(trimmed);
        }
        catch {
            sendError(0, -32700, "parse error");
            return;
        }
        handleRequest(req);
    });
    rl.on("close", () => {
        process.exit(0);
    });
}
main().catch((err) => {
    process.stderr.write(`Fatal: ${err.message}\n`);
    process.exit(1);
});
