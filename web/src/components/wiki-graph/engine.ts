/**
 * WikiGraphEngine — WebGL force-directed graph renderer for the wiki pane.
 *
 * Rendering model follows Quartz v4's graph.inline.ts (MIT, jackyzha0/quartz):
 *   - PixiJS per-node/per-link Graphics on WebGL; d3-force only computes
 *     coordinates, a manual rAF loop syncs positions and renders.
 *   - Labels keep a constant screen size (scale = 1/k) and are culled by a
 *     degree-ranked budget + greedy collision layout — visible labels never
 *     overlap at any zoom (Obsidian-style progressive reveal).
 *   - d3-zoom (pan/zoom/pinch) + d3-drag (node drag; <500ms && ≤4px = click).
 *   - Link strength 1/min(deg) softens hub edges so hubs don't clump their
 *     neighbours; collide radius far exceeds the draw radius so nodes spread
 *     out when zoomed in.
 *   - @tweenjs/tween.js eases hover/focus alpha transitions.
 *
 * Theme swaps are hot (setTheme re-colors without rebuilding); the engine is
 * destroyed and recreated only when its container unmounts/remounts.
 */
import { Application, Container, Graphics, Text, Circle } from "pixi.js";
import {
  forceSimulation,
  forceManyBody,
  forceCenter,
  forceX,
  forceY,
  forceLink,
  forceCollide,
  type Simulation,
  type SimulationNodeDatum,
  type SimulationLinkDatum,
} from "d3-force";import { select } from "d3-selection";
import { drag, type D3DragEvent } from "d3-drag";
import { zoom, zoomIdentity, type ZoomBehavior, type ZoomTransform } from "d3-zoom";
import { Group, Tween } from "@tweenjs/tween.js";

export interface GraphNode {
  id: string;
  /** Already truncated display label (caller truncates CJK titles). */
  label: string;
  pageType: string;
}

export interface GraphEdge {
  source: string;
  target: string;
}

export interface GraphTheme {
  bg: string;
  /** Fallback node color for unknown page types. */
  node: string;
  label: string;
  edge: string;
  edgeHover: string;
  selectedBorder: string;
  /** page_type → node fill. */
  typeColors: Record<string, string>;
}

const LABEL_FONT = "Inter, 'PingFang SC', 'Microsoft YaHei', -apple-system, sans-serif";
const LABEL_FONT_SIZE = 12;
/** Focus mode: non-neighbours fade to this alpha (hover / selected). */
const FADE_ALPHA_HOVER = 0.2;
const FADE_ALPHA_SELECTED = 0.08;
/** Click = pointer moved ≤ this many CSS px between press and release. */
const CLICK_MOVE_TOLERANCE = 4;
/** Label culling: min screen radius to be a candidate + degree-rank budget. */
const LABEL_MIN_SCREEN_RADIUS = 3;
const LABEL_BUDGET = 150;
const LABEL_GAP_SCREEN = 4;
const LABEL_PAD = 2;
const K_MIN = 0.1;
const K_MAX = 4;
const FIT_PADDING = 80;
/** Harmonic containment so long springs yield to the center (compact graph). */
const CONTAIN_STRENGTH = 0.06;
/** Collide radius = draw radius × MULT + PAD: dense far away, separated close. */
const COLLIDE_RADIUS_MULT = 5;
const COLLIDE_PAD = 4;
const REPEL_STRENGTH = -100;
const LINK_DISTANCE = 30;

interface SimNode extends SimulationNodeDatum {
  id: string;
  label: string;
  pageType: string;
  /** Degree — computed from deduped edges; drives radius + label rank. */
  linkCount: number;
  radius: number;
  /** Degree rank (0 = highest) for the label budget. */
  rank: number;
  initialDragPos?: { x: number; y: number };
}

// d3 resolves link source/target to node objects after the first tick.
type SimLink = SimulationLinkDatum<SimNode>;

interface NodeRenderData {
  simulationData: SimNode;
  gfx: Graphics;
  label: Text;
  /** Logical label size (resolution-independent) for collision boxes. */
  labelW: number;
  labelH: number;
  /** Part of the current focus neighbourhood (stays at full alpha). */
  active: boolean;
}

interface LinkRenderData {
  simulationData: SimLink;
  gfx: Graphics;
  color: string;
  alpha: number;
  active: boolean;
}

type TweenHandle = { update: (time: number) => boolean; stop: () => void };

function clamp(v: number, min: number, max: number): number {
  return Math.max(min, Math.min(max, v));
}

function easeCubicOut(t: number): number {
  const u = 1 - t;
  return 1 - u * u * u;
}

/** Drop self-loops, edges with missing endpoints, and A↔B duplicates. */
function dedupeEdges(edges: GraphEdge[], nodeExists: (id: string) => boolean): GraphEdge[] {
  const seen = new Set<string>();
  const out: GraphEdge[] = [];
  for (const e of edges) {
    if (e.source === e.target) continue;
    if (!nodeExists(e.source) || !nodeExists(e.target)) continue;
    const id = e.source < e.target ? `${e.source} ${e.target}` : `${e.target} ${e.source}`;
    if (seen.has(id)) continue;
    seen.add(id);
    out.push(e);
  }
  return out;
}

/**
 * Greedy label placement: candidates in priority order; any box overlapping
 * an already-placed one is dropped, so visible labels never overlap. Pinned
 * boxes (focus neighbours) always show and reserve their space.
 */
function greedyLabelLayout(
  boxes: Array<{ x0: number; y0: number; x1: number; y1: number }>,
  pinned: boolean[],
): boolean[] {
  const placed: typeof boxes = [];
  const visible: boolean[] = new Array(boxes.length).fill(false);
  for (let i = 0; i < boxes.length; i++) {
    const b = boxes[i];
    if (!b) continue;
    let collides = false;
    for (const p of placed) {
      if (b.x0 < p.x1 && b.x1 > p.x0 && b.y0 < p.y1 && b.y1 > p.y0) {
        collides = true;
        break;
      }
    }
    if (pinned[i] || !collides) {
      visible[i] = true;
      placed.push(b);
    }
  }
  return visible;
}

export class WikiGraphEngine {
  private container: HTMLElement;
  private app!: Application;
  private canvas!: HTMLCanvasElement;
  private nodesLayer!: Container;
  private labelsLayer!: Container;
  private linkLayer!: Container;
  private selectionGfx!: Graphics;
  private resizeObserver: ResizeObserver | null = null;

  private theme: GraphTheme;
  private sim: Simulation<SimNode, SimLink> | null = null;
  private nodes: SimNode[] = [];
  private adjacency = new Map<string, Set<string>>();
  private nodeById = new Map<string, SimNode>();
  private nodeRenderData: NodeRenderData[] = [];
  private linkRenderData: LinkRenderData[] = [];

  private tweens = new Map<string, TweenHandle>();

  private hoveredNodeId: string | null = null;
  private selectedId: string | null = null;
  private focusedNeighbours = new Set<string>();

  private onNodeClick: (id: string) => void;
  private destroyed = false;

  private width = 0;
  private height = 0;

  private zoomBehavior!: ZoomBehavior<HTMLCanvasElement, unknown>;
  private currentTransform: ZoomTransform = zoomIdentity;
  private suppressZoom = false;
  private viewportAnim: {
    fromTx: number; fromTy: number; fromK: number;
    toTx: number; toTy: number; toK: number;
    startTime: number; durationMs: number;
  } | null = null;

  private hasUserInteracted = false;
  private hasFitFirstLayout = false;
  private refitTimer: ReturnType<typeof setTimeout> | null = null;
  private rafId = 0;
  /**
   * Set whenever rendered state changed outside the frame loop
   * (transform, hover/focus, resize, data) — gates the heavy rebuilds in
   * renderFrame so a settled graph stops rebuilding geometry every frame.
   */
  private renderDirty = true;
  private dragging = false;
  private dragStartTime = 0;
  private dragStartXY: { x: number; y: number } | null = null;
  /** Last k for which label scales were synced (labels stay screen-sized). */
  private lastLabelK = 0;

  private constructor(container: HTMLElement, theme: GraphTheme, onNodeClick: (id: string) => void) {
    this.container = container;
    this.theme = theme;
    this.onNodeClick = onNodeClick;
  }

  static async create(
    container: HTMLElement,
    opts: { theme: GraphTheme; onNodeClick?: (id: string) => void },
  ): Promise<WikiGraphEngine> {
    const engine = new WikiGraphEngine(container, opts.theme, opts.onNodeClick ?? (() => {}));
    await engine.init();
    return engine;
  }

  private async init(): Promise<void> {
    this.width = this.container.clientWidth || 1;
    this.height = this.container.clientHeight || 1;

    const app = new Application();
    await app.init({
      resizeTo: this.container,
      antialias: true,
      autoStart: false, // manual rAF loop (Quartz-style)
      autoDensity: true,
      background: this.theme.bg,
      preference: "webgl",
      resolution: window.devicePixelRatio,
    });
    if (this.destroyed) {
      app.destroy(true);
      return;
    }
    this.app = app;
    this.width = app.screen.width;
    this.height = app.screen.height;
    this.canvas = app.canvas as HTMLCanvasElement;
    this.canvas.style.width = "100%";
    this.canvas.style.height = "100%";
    this.container.appendChild(this.canvas);

    // 'passive': the stage itself doesn't emit, but node Graphics still hit-test.
    app.stage.eventMode = "passive";
    this.linkLayer = new Container({ zIndex: 1, isRenderGroup: true });
    this.nodesLayer = new Container({ zIndex: 2, isRenderGroup: true });
    this.labelsLayer = new Container({ zIndex: 3, isRenderGroup: true });
    app.stage.addChild(this.linkLayer, this.nodesLayer, this.labelsLayer);
    this.selectionGfx = new Graphics();
    this.nodesLayer.addChild(this.selectionGfx);

    this.bindZoomAndDrag();
    this.canvas.addEventListener("dblclick", this.onDoubleClick);
    this.rafId = requestAnimationFrame(this.renderFrame);

    this.resizeObserver = new ResizeObserver(() => {
      this.width = this.container.clientWidth;
      this.height = this.container.clientHeight;
      this.app.renderer.resize(this.width, this.height);
      this.renderDirty = true;
    });
    this.resizeObserver.observe(this.container);
  }

  // ── Public API ──

  setData(nodes: GraphNode[], edges: GraphEdge[]): void {
    if (this.destroyed) return;

    const simNodes: SimNode[] = nodes.map((n) => ({
      id: n.id,
      label: n.label,
      pageType: n.pageType,
      linkCount: 0,
      radius: 0,
      rank: 0,
    }));
    this.nodeById = new Map(simNodes.map((n) => [n.id, n]));
    const cleaned = dedupeEdges(edges, (id) => this.nodeById.has(id));
    for (const e of cleaned) {
      this.nodeById.get(e.source)!.linkCount++;
      this.nodeById.get(e.target)!.linkCount++;
    }
    // Degree rank (0 = most connected) feeds the label budget; hub radius
    // grows with the square root of its degree (Obsidian-style weight).
    const byDegree = [...simNodes].sort((a, b) => b.linkCount - a.linkCount);
    byDegree.forEach((n, i) => {
      n.rank = i;
      n.radius = (2 + Math.sqrt(n.linkCount)) * 2;
    });
    this.nodes = simNodes;
    this.adjacency = new Map(simNodes.map((n) => [n.id, new Set<string>()]));
    const simLinks: SimLink[] = cleaned.map((e) => ({
      source: this.nodeById.get(e.source)!,
      target: this.nodeById.get(e.target)!,
    }));
    for (const l of simLinks) {
      const s = l.source as SimNode;
      const t = l.target as SimNode;
      this.adjacency.get(s.id)!.add(t.id);
      this.adjacency.get(t.id)!.add(s.id);
    }

    this.rebuild(simLinks);

    if (!this.hasFitFirstLayout) {
      this.fitView();
      this.hasFitFirstLayout = true;
      this.refitTimer = setTimeout(() => {
        if (!this.hasUserInteracted && !this.destroyed) this.fitView({ animate: true });
      }, 1500);
    }
  }

  /** Hot theme swap — recolors without touching the simulation. */
  setTheme(theme: GraphTheme): void {
    this.theme = theme;
    if (this.destroyed || !this.app) return;
    this.app.renderer.background.color = theme.bg;
    for (const rd of this.nodeRenderData) {
      rd.label.style.fill = theme.label;
    }
    this.redrawAllNodes();
    this.syncSelectionRing();
  }

  /** Select a node and animate the viewport onto it (page selection sync). */
  focusNode(id: string): boolean {
    if (this.destroyed) return false;
    const node = this.nodeById.get(id);
    if (!node || node.x == null || node.y == null) return false;
    this.selectedId = id;
    this.updateFocusAndRender();
    const k = clamp(1.5, K_MIN, K_MAX);
    const tx = this.width / 2 - (node.x + this.width / 2) * k;
    const ty = this.height / 2 - (node.y + this.height / 2) * k;
    this.animateToViewport(tx, ty, k, 400);
    return true;
  }

  /** Clear selection (deselect) and un-fade — viewport untouched. */
  clearSelection(): void {
    if (this.destroyed) return;
    this.selectedId = null;
    this.updateFocusAndRender();
    this.syncSelectionRing();
  }

  fitView(opts?: { animate?: boolean }): void {
    if (this.nodes.length === 0) return;
    let minX = Infinity, maxX = -Infinity, minY = Infinity, maxY = -Infinity;
    for (const n of this.nodes) {
      if (n.x == null || n.y == null) continue;
      if (n.x < minX) minX = n.x;
      if (n.x > maxX) maxX = n.x;
      if (n.y < minY) minY = n.y;
      if (n.y > maxY) maxY = n.y;
    }
    if (!isFinite(minX)) return;
    const cx = this.width / 2;
    const cy = this.height / 2;
    // Nodes render at sim coordinates + canvas-center offset; fit over that.
    const bw = Math.max(maxX - minX, 1);
    const bh = Math.max(maxY - minY, 1);
    const k = clamp(
      Math.min((this.width - FIT_PADDING * 2) / bw, (this.height - FIT_PADDING * 2) / bh),
      K_MIN,
      K_MAX,
    );
    const tx = this.width / 2 - ((minX + cx + maxX + cx) / 2) * k;
    const ty = this.height / 2 - ((minY + cy + maxY + cy) / 2) * k;
    if (opts?.animate) {
      this.animateToViewport(tx, ty, k, 500);
    } else {
      this.setTransform(tx, ty, k);
    }
  }

  destroy(): void {
    if (this.destroyed) return;
    this.destroyed = true;
    if (this.refitTimer) clearTimeout(this.refitTimer);
    this.viewportAnim = null;
    this.tweens.forEach((t) => t.stop());
    this.tweens.clear();
    cancelAnimationFrame(this.rafId);
    this.sim?.stop();
    this.resizeObserver?.disconnect();
    this.canvas?.removeEventListener("dblclick", this.onDoubleClick);
    if (this.app) this.app.destroy(true, { children: true });
  }

  // ── Internals ──

  private nodeColor(n: SimNode): string {
    return this.theme.typeColors[n.pageType] ?? this.theme.node;
  }

  private rebuild(simLinks: SimLink[]): void {
    this.sim?.stop();
    this.lastLabelK = 0;
    this.renderDirty = true;
    this.nodeRenderData = [];
    this.linkRenderData = [];
    this.tweens.forEach((t) => t.stop());
    this.tweens.clear();
    this.nodesLayer.removeChildren().forEach((c) => c.destroy());
    this.labelsLayer.removeChildren().forEach((c) => c.destroy());
    this.linkLayer.removeChildren().forEach((c) => c.destroy());
    this.selectionGfx = new Graphics();
    this.nodesLayer.addChild(this.selectionGfx);

    const sim = forceSimulation(this.nodes)
      .force("charge", forceManyBody().strength(REPEL_STRENGTH))
      .force("center", forceCenter(0, 0))
      .force(
        "link",
        forceLink(simLinks)
          .id((d) => (d as SimNode).id)
          // 1/min(deg): hub links yield, so hubs don't clump their neighbours.
          .strength((l) => {
            const s = l.source as SimNode;
            const t = l.target as SimNode;
            return 1 / Math.max(Math.min(s.linkCount, t.linkCount), 1);
          })
          .distance(LINK_DISTANCE),
      )
      .force("x", forceX(0).strength(CONTAIN_STRENGTH))
      .force("y", forceY(0).strength(CONTAIN_STRENGTH))
      .force(
        "collide",
        forceCollide()
          .radius((d) => (d as SimNode).radius * COLLIDE_RADIUS_MULT + COLLIDE_PAD)
          .iterations(3),
      );
    // The layout range only stabilizes once the simulation settles — refit
    // (once, if the user hasn't interacted) so the initial zoom matches the
    // final layout instead of an early cramped one.
    sim.on("end", () => {
      if (!this.hasUserInteracted && !this.destroyed) this.fitView({ animate: true });
    });
    this.sim = sim;

    const dpr = window.devicePixelRatio;
    for (const n of this.nodes) {
      const gfx = new Graphics({
        interactive: true,
        eventMode: "static",
        hitArea: new Circle(0, 0, n.radius + 3),
        cursor: "pointer",
      })
        .circle(0, 0, n.radius)
        .fill({ color: this.nodeColor(n) });
      gfx.on("pointerover", () => this.onNodePointerOver(n.id));
      gfx.on("pointerleave", () => this.onNodePointerLeave());
      this.nodesLayer.addChild(gfx);

      const label = new Text({
        interactive: false,
        eventMode: "none",
        text: n.label,
        alpha: 0,
        anchor: { x: 0.5, y: 1 },
        style: {
          fontSize: LABEL_FONT_SIZE,
          fill: this.theme.label,
          fontFamily: LABEL_FONT,
        },
        // Over-rasterize so text stays sharp through zoom (label textures
        // rasterize lazily on first draw, so hidden labels cost nothing).
        resolution: dpr * 2,
      });
      this.labelsLayer.addChild(label);

      this.nodeRenderData.push({
        simulationData: n,
        gfx,
        label,
        labelW: label.width,
        labelH: label.height,
        active: false,
      });
    }

    for (const l of simLinks) {
      const gfx = new Graphics({ interactive: false, eventMode: "none" });
      this.linkLayer.addChild(gfx);
      this.linkRenderData.push({
        simulationData: l,
        gfx,
        color: this.theme.edge,
        alpha: 1,
        active: false,
      });
    }

    this.updateFocusAndRender();
  }

  private redrawAllNodes(): void {
    for (const rd of this.nodeRenderData) {
      const d = rd.simulationData;
      rd.gfx.clear();
      rd.gfx.circle(0, 0, d.radius).fill({ color: this.nodeColor(d) });
    }
  }

  // ── Hover / focus ──

  private onNodePointerOver(id: string): void {
    if (this.dragging) return;
    this.hoveredNodeId = id;
    this.updateFocusAndRender();
  }

  private onNodePointerLeave(): void {
    if (this.dragging) return;
    this.hoveredNodeId = null;
    this.updateFocusAndRender();
  }

  private get focusId(): string | null {
    return this.hoveredNodeId ?? this.selectedId;
  }

  private updateFocus(): void {
    const focusId = this.focusId;
    const neighbours = new Set<string>();
    if (focusId) {
      neighbours.add(focusId);
      const adj = this.adjacency.get(focusId);
      if (adj) for (const id of adj) neighbours.add(id);
    }
    this.focusedNeighbours = neighbours;
    for (const rd of this.nodeRenderData) {
      rd.active = neighbours.has(rd.simulationData.id);
    }
    for (const rd of this.linkRenderData) {
      const s = rd.simulationData.source as SimNode;
      const t = rd.simulationData.target as SimNode;
      rd.active = s.id === focusId || t.id === focusId;
    }
  }

  private updateFocusAndRender(): void {
    this.updateFocus();
    const focusId = this.focusId;
    const dim = focusId === this.selectedId ? FADE_ALPHA_SELECTED : FADE_ALPHA_HOVER;

    this.tweens.get("hover")?.stop();
    const nodeGroup = new Group();
    for (const rd of this.nodeRenderData) {
      const alpha = focusId === null ? 1 : rd.active ? 1 : dim;
      nodeGroup.add(new Tween(rd.gfx).to({ alpha }, 200));
    }
    nodeGroup.getAll().forEach((tw) => tw.start());
    this.tweens.set("hover", this.wrapTween(nodeGroup));

    this.tweens.get("link")?.stop();
    const linkGroup = new Group();
    for (const rd of this.linkRenderData) {
      const alpha = focusId === null ? 1 : rd.active ? 1 : 0.2;
      rd.color = rd.active ? this.theme.edgeHover : this.theme.edge;
      linkGroup.add(new Tween(rd).to({ alpha }, 200));
    }
    linkGroup.getAll().forEach((tw) => tw.start());
    this.tweens.set("link", this.wrapTween(linkGroup));

    this.renderDirty = true;
  }

  /**
   * Label culling + de-overlap. Labels keep a constant screen size
   * (scale = 1/k), so their stage-local collision boxes shrink as you zoom
   * in — more labels fit, revealing progressively (Obsidian behaviour).
   * Candidates: focus neighbours (always) ∪ (screen radius ≥ threshold AND
   * degree rank within budget); greedy placement then drops every box that
   * overlaps an already-placed one.
   */
  private syncLabels(): void {
    const focusId = this.focusId;
    const dim = focusId === this.selectedId ? FADE_ALPHA_SELECTED : FADE_ALPHA_HOVER;
    const k = this.currentTransform.k;
    const cx = this.width / 2;
    const cy = this.height / 2;

    if (k !== this.lastLabelK) {
      this.lastLabelK = k;
      const s = 1 / k;
      for (const rd of this.nodeRenderData) rd.label.scale.set(s, s);
    }

    const focused: NodeRenderData[] = [];
    const normal: NodeRenderData[] = [];
    for (const rd of this.nodeRenderData) {
      const d = rd.simulationData;
      if (d.x == null || d.y == null) {
        rd.label.visible = false;
        continue;
      }
      if (focusId !== null && this.focusedNeighbours.has(d.id)) {
        focused.push(rd);
        continue;
      }
      rd.label.visible = false; // hidden until the collision pass admits it
      if (d.radius * k >= LABEL_MIN_SCREEN_RADIUS && d.rank < LABEL_BUDGET) {
        normal.push(rd);
      }
    }
    // Higher-degree labels win the budget.
    normal.sort((a, b) => a.simulationData.rank - b.simulationData.rank);

    const ordered = focused.concat(normal);
    const boxes = ordered.map((rd) => {
      const d = rd.simulationData;
      const lx = d.x! + cx;
      const top = d.y! + cy - d.radius - LABEL_GAP_SCREEN / k;
      const w = rd.labelW / k;
      const h = rd.labelH / k;
      const pad = LABEL_PAD / k;
      return { x0: lx - w / 2 - pad, x1: lx + w / 2 + pad, y0: top - h - pad, y1: top + pad };
    });
    const visible = greedyLabelLayout(
      boxes,
      ordered.map((_, i) => i < focused.length),
    );

    for (let i = 0; i < ordered.length; i++) {
      const rd = ordered[i]!;
      rd.label.visible = visible[i]!;
      rd.label.alpha = focusId !== null && !rd.active ? dim : 1;
    }
  }

  private syncSelectionRing(): void {
    const node = this.selectedId ? this.nodeById.get(this.selectedId) : null;
    const cx = this.width / 2;
    const cy = this.height / 2;
    this.selectionGfx.clear();
    if (node && node.x != null && node.y != null) {
      this.selectionGfx
        .circle(node.x + cx, node.y + cy, node.radius + 2)
        .stroke({ width: 2, color: this.theme.selectedBorder });
    }
  }

  // ── Viewport ──

  private setTransform(tx: number, ty: number, k: number): void {
    this.viewportAnim = null;
    const t = zoomIdentity.translate(tx, ty).scale(k);
    this.applyTransform(t);
    this.syncD3Transform(t);
  }

  /** Silently sync d3-zoom's internal state (no user-gesture side effects). */
  private syncD3Transform(t: ZoomTransform): void {
    this.suppressZoom = true;
    this.zoomBehavior.transform(select(this.canvas), t);
    this.suppressZoom = false;
  }

  private applyTransform(t: ZoomTransform): void {
    this.currentTransform = t;
    this.app.stage.scale.set(t.k, t.k);
    this.app.stage.position.set(t.x, t.y);
    // Label layout and the selection ring are screen-space (they depend on
    // k), so a transform change always schedules the next-frame rebuild.
    this.renderDirty = true;
  }

  private animateToViewport(tx: number, ty: number, k: number, durationMs: number): void {
    const kClamped = clamp(k, K_MIN, K_MAX);
    this.viewportAnim = {
      fromTx: this.currentTransform.x,
      fromTy: this.currentTransform.y,
      fromK: this.currentTransform.k,
      toTx: tx,
      toTy: ty,
      toK: kClamped,
      startTime: performance.now(),
      durationMs,
    };
    this.hasUserInteracted = true;
  }

  private advanceViewport(): void {
    const anim = this.viewportAnim;
    if (!anim) return;
    const t = Math.min((performance.now() - anim.startTime) / anim.durationMs, 1);
    const e = easeCubicOut(t);
    const tx = anim.fromTx + (anim.toTx - anim.fromTx) * e;
    const ty = anim.fromTy + (anim.toTy - anim.fromTy) * e;
    const k = anim.fromK + (anim.toK - anim.fromK) * e;
    const next = zoomIdentity.translate(tx, ty).scale(k);
    this.applyTransform(next);
    if (t >= 1) {
      this.viewportAnim = null;
      this.syncD3Transform(next);
    }
  }

  // ── Interaction ──

  private bindZoomAndDrag(): void {
    const canvas = this.canvas;

    this.zoomBehavior = zoom<HTMLCanvasElement, unknown>()
      .extent([
        [0, 0],
        [this.width, this.height],
      ])
      .scaleExtent([K_MIN, K_MAX])
      .on("zoom", (event: { transform: ZoomTransform }) => {
        if (this.suppressZoom) return;
        this.viewportAnim = null;
        this.hasUserInteracted = true;
        this.applyTransform(event.transform);
      });

    // Drag must register BEFORE zoom: d3-zoom's mousedown always calls
    // stopImmediatePropagation, while d3-drag only stops propagation when it
    // hits a subject — so "node hit → drag node", "blank → zoom pans".
    // Datum stays unknown (matching the bare selection); the subject
    // accessor narrows to SimNode, and event.subject is typed by it.
    select(canvas).call(
      drag<HTMLCanvasElement, unknown>()
        .container(() => canvas)
        .subject((): SimNode | undefined =>
          this.hoveredNodeId ? this.nodeById.get(this.hoveredNodeId) : undefined,
        )
        .on("start", (event: D3DragEvent<HTMLCanvasElement, unknown, SimNode | undefined>) => {
          const subj = event.subject;
          if (!subj) return;
          if (!event.active) this.sim?.alphaTarget(0.3).restart();
          subj.fx = subj.x;
          subj.fy = subj.y;
          subj.initialDragPos = { x: subj.x ?? 0, y: subj.y ?? 0 };
          this.dragStartTime = Date.now();
          this.dragStartXY = { x: event.x, y: event.y };
          this.dragging = true;
        })
        .on("drag", (event: D3DragEvent<HTMLCanvasElement, unknown, SimNode | undefined>) => {
          const subj = event.subject;
          if (!subj || !subj.initialDragPos) return;
          // Compensate zoom: screen delta / k → graph delta.
          subj.fx = subj.initialDragPos.x + (event.x - subj.initialDragPos.x) / this.currentTransform.k;
          subj.fy = subj.initialDragPos.y + (event.y - subj.initialDragPos.y) / this.currentTransform.k;
        })
        .on("end", (event: D3DragEvent<HTMLCanvasElement, unknown, SimNode | undefined>) => {
          const subj = event.subject;
          if (!subj) return;
          if (!event.active) this.sim?.alphaTarget(0);
          subj.fx = null;
          subj.fy = null;
          this.dragging = false;
          const moved = this.dragStartXY
            ? Math.hypot(event.x - this.dragStartXY.x, event.y - this.dragStartXY.y)
            : Infinity;
          this.dragStartXY = null;
          // Click (short + barely moved) selects the page — the three-pane
          // interlink. Highlighting is handled by hover / focusNode.
          if (Date.now() - this.dragStartTime < 500 && moved <= CLICK_MOVE_TOLERANCE) {
            this.onNodeClick(subj.id);
          }
        }),
    );
    select(canvas).call(this.zoomBehavior);
    // Disable d3-zoom's default double-click zoom; blank dblclick fits below.
    select(canvas).on("dblclick.zoom", null);
  }

  private onDoubleClick = (e: MouseEvent): void => {
    if (this.hitTest(e)) return;
    this.hasUserInteracted = true;
    this.fitView({ animate: true });
  };

  private hitTest(e: MouseEvent): SimNode | null {
    const rect = this.canvas.getBoundingClientRect();
    const gx = (e.clientX - rect.left - this.currentTransform.x) / this.currentTransform.k - this.width / 2;
    const gy = (e.clientY - rect.top - this.currentTransform.y) / this.currentTransform.k - this.height / 2;
    let best: SimNode | null = null;
    let bestDist = Infinity;
    for (const n of this.nodeById.values()) {
      if (n.x == null || n.y == null) continue;
      const dist = Math.hypot(n.x - gx, n.y - gy);
      if (dist < Math.max(n.radius * 1.5, 8) && dist < bestDist) {
        bestDist = dist;
        best = n;
      }
    }
    return best;
  }

  // ── Render loop ──

  private readonly renderFrame = (time: number): void => {
    if (this.destroyed) return;
    this.advanceViewport();
    let tweenAlive = false;
    for (const t of this.tweens.values()) tweenAlive = t.update(time) || tweenAlive;

    // Idle gate: the heavy rebuilds (node positions, link geometry, label
    // collision layout, selection ring) only run while something is
    // actually moving — the physics sim, a viewport or alpha-tween
    // animation, a drag, or an explicit dirty flag (transform / hover /
    // focus / resize changes). A settled graph skips straight to the GPU
    // submit instead of rebuilding ~N links and running the O(budget²)
    // label layout 60×/s while idle.
    // d3-force has no "is running" accessor: while the internal timer is
    // alive alpha() sits at/above alphaMin(); once it cools below, tick
    // timers stop and positions are settled.
    const simAlive = this.sim != null && this.sim.alpha() >= this.sim.alphaMin();
    const busy =
      this.renderDirty ||
      tweenAlive ||
      this.viewportAnim !== null ||
      this.dragging ||
      simAlive;
    if (busy) {
      this.renderDirty = false;
      const cx = this.width / 2;
      const cy = this.height / 2;
      for (const rd of this.nodeRenderData) {
        const d = rd.simulationData;
        if (d.x == null || d.y == null) continue;
        const x = d.x + cx;
        const y = d.y + cy;
        rd.gfx.position.set(x, y);
        rd.label.position.set(x, y - d.radius - LABEL_GAP_SCREEN / this.currentTransform.k);
      }
      for (const rd of this.linkRenderData) {
        const d = rd.simulationData;
        const s = d.source as SimNode;
        const t = d.target as SimNode;
        if (s.x == null || s.y == null || t.x == null || t.y == null) continue;
        rd.gfx.clear();
        rd.gfx
          .moveTo(s.x + cx, s.y + cy)
          .lineTo(t.x + cx, t.y + cy)
          .stroke({ alpha: rd.alpha, width: 1, color: rd.color });
      }

      this.syncLabels();
      this.syncSelectionRing();
    }

    this.app.renderer.render(this.app.stage);
    this.rafId = requestAnimationFrame(this.renderFrame);
  };

  private wrapTween(group: Group): TweenHandle {
    return {
      // Older tween.js Group.update returns void — probe the tweens for
      // liveness instead (any still playing keeps the render loop busy).
      update: (time: number) => {
        group.update(time);
        return group.getAll().some((tw) => tw.isPlaying());
      },
      stop() {
        group.getAll().forEach((tw) => tw.stop());
      },
    };
  }
}
