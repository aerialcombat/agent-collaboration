/**
 * peer-inbox extension for pi — live integration with agent-collaboration.
 *
 * Usage from pi TUI:
 *   /peer-join <label> [pair-key]   register + start listening
 *   /peer-leave                     tear down listener, clear registration
 *   /peer-status                    show current registration
 *   /peer-send <to> <message...>    DM a peer
 *   /peer-broadcast <message...>    broadcast to current room
 *
 * When joined, inbound peer-inbox pushes arrive as wrapped user turns:
 *   <peer-inbox from="X" ...>...body...</peer-inbox>
 * and pi's assistant replies auto-relay back to the envelope sender via
 * `agent-collab peer send`.
 */

import type { ExtensionAPI } from "@mariozechner/pi-coding-agent";
import { Type } from "@sinclair/typebox";
import * as net from "node:net";
import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { execFile, execFileSync } from "node:child_process";

const DB_SCRIPT =
	process.env.AGENT_COLLAB_PEER_INBOX_DB_SCRIPT ||
	path.join(os.homedir(), ".agent-collab/scripts/peer-inbox-db.py");
const INBOX_DB =
	process.env.AGENT_COLLAB_INBOX_DB ||
	path.join(os.homedir(), ".agent-collab/sessions.db");

interface State {
	label: string;
	pairKey: string | null;
	socketPath: string;
	server: net.Server;
	cwd: string;
}

function parseHttpRequest(data: Buffer) {
	const sep = Buffer.from("\r\n\r\n");
	const idx = data.indexOf(sep);
	if (idx < 0) return null;
	const head = data.subarray(0, idx).toString("utf-8");
	const rest = data.subarray(idx + 4);
	const lines = head.split("\r\n");
	const method = (lines[0] || "").split(" ")[0] || "";
	const headers: Record<string, string> = {};
	for (const line of lines.slice(1)) {
		const i = line.indexOf(":");
		if (i > 0) headers[line.slice(0, i).trim().toLowerCase()] = line.slice(i + 1).trim();
	}
	const len = parseInt(headers["content-length"] || "0", 10);
	if (rest.length < len) return null;
	return { method, headers, body: rest.subarray(0, len) };
}

function writeHttpResponse(conn: net.Socket, code: number, payload: unknown) {
	const body = Buffer.from(JSON.stringify(payload));
	const head =
		`HTTP/1.1 ${code} OK\r\n` +
		`Content-Type: application/json\r\n` +
		`Content-Length: ${body.length}\r\n` +
		`Connection: close\r\n\r\n`;
	try { conn.write(Buffer.concat([Buffer.from(head), body])); } catch { /* ignore */ }
}

function updateChannelSocket(cwd: string, label: string, value: string | null) {
	const sql = value === null
		? "UPDATE sessions SET channel_socket=NULL WHERE cwd=? AND label=?"
		: "UPDATE sessions SET channel_socket=? WHERE cwd=? AND label=?";
	const args = value === null
		? [INBOX_DB, cwd, label]
		: [INBOX_DB, value, cwd, label];
	const script =
		`import sqlite3,sys\n` +
		`c=sqlite3.connect(sys.argv[1])\n` +
		`c.execute(${JSON.stringify(sql)}, sys.argv[2:])\n` +
		`c.commit(); c.close()\n`;
	execFileSync("python3", ["-c", script, ...args], { timeout: 5000 });
}

export default function (pi: ExtensionAPI) {
	let state: State | null = null;
	let currentUserSender: string | null = null;

	const teardown = () => {
		if (!state) return;
		const { server, socketPath, cwd, label } = state;
		state = null;
		try { server.close(); } catch { /* ignore */ }
		try { fs.unlinkSync(socketPath); } catch { /* ignore */ }
		try { updateChannelSocket(cwd, label, null); } catch { /* ignore */ }
	};

	const join = async (label: string, pairKey: string | null, notify: (m: string, l?: "info" | "warning" | "error") => void) => {
		if (state) {
			notify(`already joined as ${state.label}. /peer-leave first.`, "warning");
			return;
		}
		const socketPath = `/tmp/peer-inbox-pi-${process.pid}-${label}.sock`;
		try { fs.unlinkSync(socketPath); } catch { /* ignore */ }

		const server = net.createServer((conn) => {
			let buf = Buffer.alloc(0);
			let done = false;
			conn.on("data", (chunk) => {
				if (done) return;
				buf = Buffer.concat([buf, chunk]);
				const req = parseHttpRequest(buf);
				if (!req) return;
				done = true;
				if (req.method === "GET") {
					writeHttpResponse(conn, 200, { server: "peer-inbox-pi-extension", initialized: true });
					conn.end();
					return;
				}
				if (req.method !== "POST") {
					writeHttpResponse(conn, 405, { error: "method not allowed" });
					conn.end();
					return;
				}
				let payload: { body?: string; content?: string; from?: string; meta?: Record<string, unknown> } = {};
				try { payload = JSON.parse(req.body.toString("utf-8") || "{}"); }
				catch { writeHttpResponse(conn, 400, { error: "body must be JSON" }); conn.end(); return; }
				const content = payload.body || payload.content || "";
				const sender = payload.from || req.headers["x-sender"] || "";
				const meta = (payload.meta && typeof payload.meta === "object" ? payload.meta : {}) as Record<string, unknown>;
				if (!content || !sender) {
					writeHttpResponse(conn, 400, { error: "from + body required" });
					conn.end();
					return;
				}
				const metaAttrs = Object.entries(meta)
					.map(([k, v]) => ` ${k}="${String(v).replace(/"/g, "'")}"`)
					.join("");
				const envelope = `<peer-inbox from="${sender}"${metaAttrs}>\n${content}\n</peer-inbox>`;
				try {
					pi.sendUserMessage(envelope, { deliverAs: "followUp" });
					writeHttpResponse(conn, 200, { ok: true });
				} catch (err) {
					const msg = err instanceof Error ? err.message : String(err);
					writeHttpResponse(conn, 500, { error: msg });
				}
				conn.end();
			});
			conn.on("error", () => { /* swallow */ });
		});
		server.on("error", (err) => notify(`socket error: ${err.message}`, "warning"));

		await new Promise<void>((resolve, reject) => {
			server.once("error", reject);
			server.listen(socketPath, () => {
				try { fs.chmodSync(socketPath, 0o600); } catch { /* ignore */ }
				resolve();
			});
		});

		const cwd = process.cwd();
		const registerArgs = [
			DB_SCRIPT, "session-register",
			"--cwd", cwd,
			"--label", label,
			"--agent", "pi",
			"--role", "peer",
			"--session-key", `pi-ext-${process.pid}-${Date.now()}`,
			"--force",
		];
		if (pairKey) registerArgs.push("--pair-key", pairKey);
		try { execFileSync("python3", registerArgs, { timeout: 10000, encoding: "utf-8" }); }
		catch (err) {
			try { server.close(); } catch { /* ignore */ }
			try { fs.unlinkSync(socketPath); } catch { /* ignore */ }
			notify(`register failed: ${err instanceof Error ? err.message : String(err)}`, "error");
			return;
		}

		try { updateChannelSocket(cwd, label, socketPath); }
		catch (err) {
			notify(`channel_socket update failed: ${err instanceof Error ? err.message : String(err)}`, "warning");
		}

		state = { label, pairKey, socketPath, server, cwd };
		notify(`joined as ${label}${pairKey ? ` (pair_key=${pairKey})` : ""} on ${socketPath}`, "info");
	};

	pi.registerCommand("peer-join", {
		description: "register + open peer-inbox channel: /peer-join <label> [pair-key]",
		handler: async (args, ctx) => {
			const parts = args.trim().split(/\s+/).filter(Boolean);
			if (parts.length < 1) { ctx.ui.notify("usage: /peer-join <label> [pair-key]", "warning"); return; }
			const [label, pairKey] = parts;
			await join(label, pairKey || null, ctx.ui.notify.bind(ctx.ui));
		},
	});

	pi.registerCommand("peer-leave", {
		description: "tear down peer-inbox channel",
		handler: async (_args, ctx) => {
			if (!state) { ctx.ui.notify("not joined", "warning"); return; }
			const label = state.label;
			teardown();
			ctx.ui.notify(`left ${label}`, "info");
		},
	});

	pi.registerCommand("peer-status", {
		description: "show peer-inbox registration",
		handler: async (_args, ctx) => {
			if (!state) { ctx.ui.notify("not joined", "info"); return; }
			ctx.ui.notify(
				`label=${state.label} pair_key=${state.pairKey || "-"} socket=${state.socketPath}`,
				"info",
			);
		},
	});

	pi.registerCommand("peer-send", {
		description: "DM a peer: /peer-send <label> <message...>",
		handler: async (args, ctx) => {
			if (!state) { ctx.ui.notify("not joined; /peer-join first", "warning"); return; }
			const trimmed = args.trim();
			const i = trimmed.indexOf(" ");
			if (i < 0) { ctx.ui.notify("usage: /peer-send <label> <message>", "warning"); return; }
			const to = trimmed.slice(0, i);
			const message = trimmed.slice(i + 1);
			try {
				execFileSync("python3", [
					DB_SCRIPT, "peer-send",
					"--cwd", state.cwd,
					"--as", state.label,
					"--to", to,
					"--message", message,
				], { timeout: 10000 });
				ctx.ui.notify(`sent to ${to}`, "info");
			} catch (err) {
				ctx.ui.notify(`send failed: ${err instanceof Error ? err.message : String(err)}`, "error");
			}
		},
	});

	pi.registerCommand("peer-broadcast", {
		description: "broadcast to the room: /peer-broadcast <message...>",
		handler: async (args, ctx) => {
			if (!state) { ctx.ui.notify("not joined; /peer-join first", "warning"); return; }
			const message = args.trim();
			if (!message) { ctx.ui.notify("usage: /peer-broadcast <message>", "warning"); return; }
			try {
				execFileSync("python3", [
					DB_SCRIPT, "peer-broadcast",
					"--cwd", state.cwd,
					"--as", state.label,
					"--message", message,
				], { timeout: 10000 });
				ctx.ui.notify("broadcast sent", "info");
			} catch (err) {
				ctx.ui.notify(`broadcast failed: ${err instanceof Error ? err.message : String(err)}`, "error");
			}
		},
	});

	// Tool equivalents of the slash commands, callable by pi's LLM. Tools let
	// pi autonomously decide to send/broadcast/etc. without the operator
	// typing a /peer-* command. Same semantics, different consumer.
	pi.registerTool({
		name: "peer_join",
		label: "Peer Join",
		description: "Register this pi session with peer-inbox under a label, optionally joining a pair_key-scoped room. Opens a socket so other agents can DM this session.",
		parameters: Type.Object({
			label: Type.String({ description: "unique label for this session in the room" }),
			pair_key: Type.Optional(Type.String({ description: "pair_key to join; omit for cwd-only scope" })),
		}),
		async execute(_id, params, _signal, _onUpdate, _ctx) {
			const messages: string[] = [];
			await join(
				params.label,
				params.pair_key || null,
				(m) => { messages.push(m); },
			);
			const text = messages.join("\n") || (state ? `joined as ${state.label}` : "join failed");
			return { content: [{ type: "text", text }], details: { state } };
		},
	});

	pi.registerTool({
		name: "peer_leave",
		label: "Peer Leave",
		description: "Leave the peer-inbox channel: tear down listener and clear the session's channel_socket registration.",
		parameters: Type.Object({}),
		async execute(_id, _params, _signal, _onUpdate, _ctx) {
			if (!state) return { content: [{ type: "text", text: "not joined" }] };
			const label = state.label;
			teardown();
			return { content: [{ type: "text", text: `left ${label}` }] };
		},
	});

	pi.registerTool({
		name: "peer_status",
		label: "Peer Status",
		description: "Report the current peer-inbox registration (label, pair_key, socket path) or 'not joined'.",
		parameters: Type.Object({}),
		async execute(_id, _params, _signal, _onUpdate, _ctx) {
			if (!state) return { content: [{ type: "text", text: "not joined" }] };
			const text = `label=${state.label} pair_key=${state.pairKey || "-"} socket=${state.socketPath}`;
			return { content: [{ type: "text", text }], details: { state } };
		},
	});

	pi.registerTool({
		name: "peer_send",
		label: "Peer Send",
		description: "DM another peer in the current room. Use when the user asks you to message a specific labelled peer, or when a response should reach only one addressee. Requires prior peer_join.",
		promptSnippet: "DM a peer-inbox peer by label",
		promptGuidelines: [
			"Use peer_send(to, message) to DM another agent in the current peer-inbox room by label.",
			"Never run `peer-send` or `agent-collab peer send` via bash — those are slash-command / CLI forms; the tool is peer_send.",
			"If the user says 'say hi to claude2' or 'DM pi-ext', call peer_send with {to: <label>, message: <text>}.",
		],
		parameters: Type.Object({
			to: Type.String({ description: "recipient label (e.g. 'claude2', 'codex-agent')" }),
			message: Type.String({ description: "message body" }),
		}),
		async execute(_id, params, _signal, _onUpdate, _ctx) {
			if (!state) return { content: [{ type: "text", text: "error: not joined; call peer_join first" }] };
			try {
				execFileSync("python3", [
					DB_SCRIPT, "peer-send",
					"--cwd", state.cwd,
					"--as", state.label,
					"--to", params.to,
					"--message", params.message,
				], { timeout: 10000 });
				return { content: [{ type: "text", text: `sent to ${params.to}` }] };
			} catch (err) {
				const msg = err instanceof Error ? err.message : String(err);
				return { content: [{ type: "text", text: `send failed: ${msg}` }] };
			}
		},
	});

	pi.registerTool({
		name: "peer_broadcast",
		label: "Peer Broadcast",
		description: "Broadcast a message to every peer in the current room. Use when announcing something room-wide; prefer peer_send for a single addressee. Requires prior peer_join.",
		promptSnippet: "Broadcast a message to the whole peer-inbox room",
		promptGuidelines: [
			"Use peer_broadcast(message) to fan out to every peer in the current room.",
			"Prefer peer_send(to, message) when a single addressee is intended; broadcast only for room-wide announcements.",
		],
		parameters: Type.Object({
			message: Type.String({ description: "message body" }),
		}),
		async execute(_id, params, _signal, _onUpdate, _ctx) {
			if (!state) return { content: [{ type: "text", text: "error: not joined; call peer_join first" }] };
			try {
				execFileSync("python3", [
					DB_SCRIPT, "peer-broadcast",
					"--cwd", state.cwd,
					"--as", state.label,
					"--message", params.message,
				], { timeout: 10000 });
				return { content: [{ type: "text", text: "broadcast sent" }] };
			} catch (err) {
				const msg = err instanceof Error ? err.message : String(err);
				return { content: [{ type: "text", text: `broadcast failed: ${msg}` }] };
			}
		},
	});

	pi.on("message_start", async (event) => {
		const msg = (event as { message?: { role?: string; content?: Array<{ type: string; text?: string }> } }).message;
		if (!msg || msg.role !== "user") return;
		const content = msg.content || [];
		for (const c of content) {
			if (c.type === "text" && typeof c.text === "string") {
				const m = c.text.match(/from="([^"]+)"/);
				currentUserSender = m ? m[1] : null;
				break;
			}
		}
	});

	pi.on("turn_end", async (event) => {
		const msg = (event as { message?: { role?: string; content?: Array<{ type: string; text?: string }> } }).message;
		if (!state || !msg || msg.role !== "assistant" || !currentUserSender) {
			currentUserSender = null;
			return;
		}
		const content = msg.content || [];
		let text = "";
		for (const c of content) {
			if (c.type === "text" && typeof c.text === "string") {
				const t = c.text.trim();
				if (t) { text = t; break; }
			}
		}
		const dest = currentUserSender;
		currentUserSender = null;
		if (!text) return;
		execFile(
			"python3",
			[
				DB_SCRIPT, "peer-send",
				"--cwd", state.cwd,
				"--as", state.label,
				"--to", dest,
				"--message", text,
			],
			{ timeout: 15000 },
			() => { /* fire and forget */ },
		);
	});

	pi.on("session_shutdown", async () => { teardown(); });

	// Env-driven auto-join. Lets headless modes (--mode rpc, --mode json) open
	// the channel without a slash command. Interactive TUI sessions can still
	// activate via /peer-join at any time.
	pi.on("session_start", async (_event, ctx) => {
		const label = process.env.PEER_INBOX_LABEL;
		if (!label) return;
		const pairKey = process.env.PEER_INBOX_PAIR_KEY || null;
		await join(label, pairKey, (m, l) => {
			if (ctx.hasUI) ctx.ui.notify(m, l);
			else console.error(`[peer-inbox-pi] ${l || "info"}: ${m}`);
		});
	});
}
