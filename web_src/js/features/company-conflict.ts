// VSCode-style resolution UI for git conflict markers inside the company
// workspace editor (company-workspace.ts). The conflict itself is plain
// marker text in the document — <<<<<<< / ======= / >>>>>>> — inserted by
// handleConflict/recoverPendingEdits, so hand-editing always works; this
// extension renders each block with tinted sections and one-click
// "keep mine / use saved / keep both" buttons above it for people who've
// never resolved a git conflict by hand. Attached after createCodeEditor
// via appendConfig so the core codeeditor module stays untouched.
import {RangeSetBuilder, StateEffect, StateField} from '@codemirror/state';
import type {EditorState, Extension} from '@codemirror/state';
import {Decoration, EditorView, WidgetType} from '@codemirror/view';
import type {DecorationSet} from '@codemirror/view';

export type ConflictLabels = {
  useYours: string,
  useTheirs: string,
  useBoth: string,
};

// 1-based line numbers of one block's three marker lines. Only ever read
// against the exact doc state the decoration pass that produced it saw —
// any doc change rebuilds all decorations (and these with them), and
// resolve() below re-verifies the lines before touching anything.
type ConflictBlock = {start: number, sep: number, end: number};

const START = '<<<<<<< ';
const SEP = '=======';
const END = '>>>>>>> ';

function parseBlocks(state: EditorState): ConflictBlock[] {
  const blocks: ConflictBlock[] = [];
  const lineCount = state.doc.lines;
  for (let n = 1; n <= lineCount; n++) {
    if (!state.doc.line(n).text.startsWith(START)) continue;
    let sep = 0;
    for (let m = n + 1; m <= lineCount; m++) {
      const text = state.doc.line(m).text;
      if (!sep && text === SEP) {
        sep = m;
      } else if (sep && text.startsWith(END)) {
        blocks.push({start: n, sep, end: m});
        n = m; // resume scanning after this block
        break;
      } else if (text.startsWith(START)) {
        break; // malformed (new block before this one closed) — skip, hand-editing can still fix it
      }
    }
  }
  return blocks;
}

function sectionText(state: EditorState, afterLine: number, beforeLine: number): string {
  // everything between two marker lines, trailing newline included ('' when adjacent)
  return state.sliceDoc(state.doc.line(afterLine).to + 1, state.doc.line(beforeLine).from);
}

function resolve(view: EditorView, block: ConflictBlock, choice: 'yours' | 'theirs' | 'both'): void {
  const {state} = view;
  // The doc this block was parsed from is the doc these decorations are
  // rendered against, so this only ever fails if something got out of
  // sync — then doing nothing beats corrupting content.
  if (block.end > state.doc.lines ||
    !state.doc.line(block.start).text.startsWith(START) ||
    state.doc.line(block.sep).text !== SEP ||
    !state.doc.line(block.end).text.startsWith(END)) return;

  const yours = sectionText(state, block.start, block.sep);
  const theirs = sectionText(state, block.sep, block.end);
  let replacement = choice === 'yours' ? yours : choice === 'theirs' ? theirs : yours + theirs;

  const from = state.doc.line(block.start).from;
  const endLine = state.doc.line(block.end);
  let to = endLine.to;
  if (to < state.doc.length) {
    to++; // swallow the end marker's own newline too
  } else {
    replacement = replacement.replace(/\n$/, ''); // block closed the file without a trailing newline — don't introduce one
  }
  view.dispatch({changes: {from, to, insert: replacement}});

  const {state: stateAfter} = view; // post-dispatch state, not the one this block was parsed from
  if (!parseBlocks(stateAfter).length) {
    // the workspace page listens for this to clear its conflict status line
    view.dom.dispatchEvent(new CustomEvent('company-conflict-resolved', {bubbles: true}));
  }
}

class ResolveButtonsWidget extends WidgetType {
  block: ConflictBlock;
  labels: ConflictLabels;

  constructor(block: ConflictBlock, labels: ConflictLabels) {
    super();
    this.block = block;
    this.labels = labels;
  }

  override eq(other: ResolveButtonsWidget): boolean {
    return other.block.start === this.block.start && other.block.sep === this.block.sep && other.block.end === this.block.end;
  }

  override toDOM(view: EditorView): HTMLElement {
    const wrap = document.createElement('div');
    wrap.className = 'company-conflict-actions';
    const options: Array<[string, 'yours' | 'theirs' | 'both']> = [
      [this.labels.useYours, 'yours'],
      [this.labels.useTheirs, 'theirs'],
      [this.labels.useBoth, 'both'],
    ];
    for (const [label, choice] of options) {
      const btn = document.createElement('button');
      btn.type = 'button';
      btn.textContent = label;
      btn.addEventListener('click', (e) => {
        e.preventDefault();
        resolve(view, this.block, choice);
      });
      wrap.append(btn);
    }
    return wrap;
  }
}

function buildDecorations(state: EditorState, labels: ConflictLabels): DecorationSet {
  const builder = new RangeSetBuilder<Decoration>();
  const markerLine = Decoration.line({class: 'company-conflict-marker'});
  const yoursLine = Decoration.line({class: 'company-conflict-yours'});
  const theirsLine = Decoration.line({class: 'company-conflict-theirs'});
  for (const block of parseBlocks(state)) {
    const startFrom = state.doc.line(block.start).from;
    builder.add(startFrom, startFrom, Decoration.widget({widget: new ResolveButtonsWidget(block, labels), block: true, side: -1}));
    builder.add(startFrom, startFrom, markerLine);
    for (let n = block.start + 1; n < block.sep; n++) {
      const {from} = state.doc.line(n);
      builder.add(from, from, yoursLine);
    }
    const sepFrom = state.doc.line(block.sep).from;
    builder.add(sepFrom, sepFrom, markerLine);
    for (let n = block.sep + 1; n < block.end; n++) {
      const {from} = state.doc.line(n);
      builder.add(from, from, theirsLine);
    }
    const endFrom = state.doc.line(block.end).from;
    builder.add(endFrom, endFrom, markerLine);
  }
  return builder.finish();
}

function conflictExtension(labels: ConflictLabels): Extension {
  return StateField.define<DecorationSet>({
    create: (state) => buildDecorations(state, labels),
    update: (deco, tr) => tr.docChanged ? buildDecorations(tr.state, labels) : deco,
    provide: (field) => EditorView.decorations.from(field),
  });
}

// Matches any of the three marker lines anywhere in a file — the Save
// handler uses this to warn before committing unresolved markers as-is.
export const conflictMarkerPattern = /^(?:<{7} |={7}$|>{7} )/m;

export function attachConflictUI(view: EditorView, labels: ConflictLabels): void {
  view.dispatch({effects: StateEffect.appendConfig.of(conflictExtension(labels))});
}

// ---- building conflict text: mark only the lines that actually differ ----
//
// Wrapping the whole file in one marker block made every conflict look
// total — the reader couldn't tell which lines actually disagreed. This
// merges like git does instead: a line-level diff keeps common lines as
// plain text and wraps only diverging runs in marker blocks, and when the
// common ancestor is known (the save-time 409 carries it) a 3-way merge
// silently combines edits that touched different lines, leaving a
// conflict block only where both sides changed the same lines.

type MergeSegment =
  | {kind: 'text', lines: string[]} |
  {kind: 'conflict', yours: string[], theirs: string[]};

export type MergeResult = {text: string, conflicts: number};

function splitLines(s: string): string[] {
  if (s === '') return [];
  const lines = s.split('\n');
  if (lines.at(-1) === '') lines.pop(); // trailing newline, not an extra empty line
  return lines;
}

// Longest-common-subsequence line pairs via the classic DP table; null
// when the quadratic table would get too big — callers then fall back to
// one whole-file conflict block, which is always correct, just coarser.
function lcsPairs(a: string[], b: string[]): Array<[number, number]> | null {
  if ((a.length + 1) * (b.length + 1) > 4_000_000) return null;
  const width = b.length + 1;
  const dp = new Uint32Array((a.length + 1) * width);
  for (let i = a.length - 1; i >= 0; i--) {
    for (let j = b.length - 1; j >= 0; j--) {
      dp[i * width + j] = a[i] === b[j] ? dp[(i + 1) * width + j + 1] + 1 : Math.max(dp[(i + 1) * width + j], dp[i * width + j + 1]);
    }
  }
  const pairs: Array<[number, number]> = [];
  let i = 0;
  let j = 0;
  while (i < a.length && j < b.length) {
    if (a[i] === b[j]) {
      pairs.push([i, j]);
      i++;
      j++;
    } else if (dp[(i + 1) * width + j] >= dp[i * width + j + 1]) {
      i++;
    } else {
      j++;
    }
  }
  return pairs;
}

function sameLines(a: string[], b: string[]): boolean {
  return a.length === b.length && a.every((line, n) => line === b[n]);
}

function pushText(segments: MergeSegment[], lines: string[]): void {
  if (!lines.length) return;
  const last = segments.at(-1);
  if (last?.kind === 'text') last.lines.push(...lines);
  else segments.push({kind: 'text', lines: [...lines]});
}

// Classic diff3: walk the base; where both sides still match it, the line
// is common; between such anchors, a side that left its chunk untouched
// yields to the side that changed it, and only both-changed-differently
// chunks become conflicts.
function diff3Segments(base: string[], yours: string[], theirs: string[]): MergeSegment[] | null {
  const pairsYours = lcsPairs(base, yours);
  const pairsTheirs = lcsPairs(base, theirs);
  if (!pairsYours || !pairsTheirs) return null;
  const mapYours = new Map(pairsYours);
  const mapTheirs = new Map(pairsTheirs);
  const segments: MergeSegment[] = [];
  let i = 0; // base cursor
  let j = 0; // yours cursor
  let k = 0; // theirs cursor
  while (i < base.length || j < yours.length || k < theirs.length) {
    if (i < base.length && mapYours.get(i) === j && mapTheirs.get(i) === k) {
      pushText(segments, [base[i]]);
      i++;
      j++;
      k++;
      continue;
    }
    // unstable chunk — ends at the next base line still matched on both sides
    let s = i;
    let t = yours.length;
    let u = theirs.length;
    for (; s < base.length; s++) {
      const inYours = mapYours.get(s);
      const inTheirs = mapTheirs.get(s);
      if (inYours !== undefined && inTheirs !== undefined && inYours >= j && inTheirs >= k) {
        t = inYours;
        u = inTheirs;
        break;
      }
    }
    const baseChunk = base.slice(i, s);
    const yoursChunk = yours.slice(j, t);
    const theirsChunk = theirs.slice(k, u);
    if (sameLines(yoursChunk, baseChunk)) {
      pushText(segments, theirsChunk); // only they changed it — take theirs silently
    } else if (sameLines(theirsChunk, baseChunk) || sameLines(yoursChunk, theirsChunk)) {
      pushText(segments, yoursChunk); // only we changed it (or both made the identical change)
    } else {
      segments.push({kind: 'conflict', yours: yoursChunk, theirs: theirsChunk});
    }
    i = s;
    j = t;
    k = u;
  }
  return segments;
}

// No ancestor known (recovery after refresh): a 2-way diff can't tell who
// changed what, but it still pins each divergence to its actual lines.
function diff2Segments(yours: string[], theirs: string[]): MergeSegment[] | null {
  const pairs = lcsPairs(yours, theirs);
  if (!pairs) return null;
  const segments: MergeSegment[] = [];
  let j = 0;
  let k = 0;
  for (const [pj, pk] of pairs) {
    if (j < pj || k < pk) segments.push({kind: 'conflict', yours: yours.slice(j, pj), theirs: theirs.slice(k, pk)});
    pushText(segments, [yours[pj]]);
    j = pj + 1;
    k = pk + 1;
  }
  if (j < yours.length || k < theirs.length) segments.push({kind: 'conflict', yours: yours.slice(j), theirs: theirs.slice(k)});
  return segments;
}

// The merged document: common/auto-merged lines plain, each genuinely
// conflicting run wrapped in markers (the exact format parseBlocks above
// reads back). conflicts === 0 means everything merged cleanly — no
// markers, nothing for the person to resolve.
export function buildConflictText(base: string | null, yours: string, theirs: string, yoursLabel: string, theirsLabel: string): MergeResult {
  const yourLines = splitLines(yours);
  const theirLines = splitLines(theirs);
  let segments = base === null ? diff2Segments(yourLines, theirLines) : diff3Segments(splitLines(base), yourLines, theirLines);
  segments ??= [{kind: 'conflict', yours: yourLines, theirs: theirLines}];
  const out: string[] = [];
  let conflicts = 0;
  for (const segment of segments) {
    if (segment.kind === 'text') {
      out.push(...segment.lines);
    } else {
      conflicts++;
      out.push(`${START}${yoursLabel}`, ...segment.yours, SEP, ...segment.theirs, `${END}${theirsLabel}`);
    }
  }
  return {text: out.length ? `${out.join('\n')}\n` : '', conflicts};
}
