import {registerGlobalInitFunc} from '../modules/observer.ts';
import {createCodeEditor} from '../modules/codeeditor/main.ts';
import type {CodemirrorEditor} from '../modules/codeeditor/main.ts';
import {createElementFromHTML} from '../utils/dom.ts';
import {showErrorToast} from '../modules/toast.ts';
import {GET, POST} from '../modules/fetch.ts';
import {svg} from '../svg.ts';
import {initChatPanel} from './company-ai-chat.ts';
import type {ChatPanelHandle} from './company-ai-chat.ts';

// Multi-file editor for company/workspace.tmpl. Reuses Gitea's own
// CodeMirror setup (modules/codeeditor) verbatim — one instance per open
// tab, same way the native single-file editor uses one instance per page
// — and Gitea's own existing tree-list/raw routes for listing and reading
// files, so nothing new is added to the Go side except the "save several
// files as one commit" endpoint (company/workspace.go). See
// docs/company/repo-ui.md.

type OpenTab = {
  path: string; // current working path — mutable, changes when dragged into a different folder
  // Where this content currently sits on the server, or null if it never
  // has (a brand-new "새 파일"). Drag-and-drop only updates `path`, not
  // this — so path !== serverPath is exactly "needs a rename on next
  // save", and serverPath itself is what a delete instruction targets
  // (the file may have been renamed-but-not-saved before being deleted).
  serverPath: string | null;
  textarea: HTMLTextAreaElement;
  pane: HTMLElement;
  tabEl: HTMLElement;
  originalContent: string;
  // Set once createCodeEditor resolves (openFile itself awaits it, so this
  // is only ever unset for the brief moment a tab is still loading).
  // CodeMirror never observes external writes to textarea.value on its
  // own — an AI-proposed edit landing on an already-open tab (applyAIEdit
  // below) has to go through view.dispatch() directly, or the on-screen
  // editor silently keeps showing the old content while the hidden
  // textarea (and so the dirty-check/Save) quietly has the new one.
  editor?: CodemirrorEditor;
  // Pending debounced write to the server's tmp staging area
  // (company/workspace_tmp.go) — tracked on the tab itself so a
  // successful Save (which supersedes any staged draft entirely) can
  // cancel it, the same reasoning as the old draftSaveTimer this
  // replaces: a keystroke right before clicking Save must not be allowed
  // to re-stage now-superseded content moments later.
  tmpSaveTimer?: ReturnType<typeof setTimeout>;
};

// File tree, VSCode-style: folders before files, each level sorted with the
// newest client-created entry at the top (server-loaded entries keep
// tree-list's own order, appended below). Nothing here is git-aware — an
// empty "new folder" only becomes real once a file is saved into it.
type FolderNode = {
  name: string;
  path: string;
  folders: FolderNode[];
  files: string[];
};

function findOrCreateFolder(parent: FolderNode, name: string, prepend: boolean): FolderNode {
  let child = parent.folders.find((f) => f.name === name);
  if (!child) {
    child = {name, path: parent.path ? `${parent.path}/${name}` : name, folders: [], files: []};
    if (prepend) parent.folders.unshift(child);
    else parent.folders.push(child);
  }
  return child;
}

function insertFilePath(root: FolderNode, path: string, prepend: boolean): void {
  const parts = path.split('/').filter(Boolean);
  if (!parts.length) return;
  let node = root;
  for (let i = 0; i < parts.length - 1; i++) node = findOrCreateFolder(node, parts[i], prepend);
  const filename = parts[parts.length - 1];
  if (node.files.includes(filename)) return;
  if (prepend) node.files.unshift(filename);
  else node.files.push(filename);
}

// Used for drag-and-drop moves specifically: a file leaving one folder for
// another lands in alphabetical position there, not at the top — "new"
// items (created via the + buttons) still go to the top, this is only for
// relocating something that already existed. Otherwise the tree would only
// look sorted after a refresh (which re-fetches from tree-list, itself
// already sorted server-side), not immediately after the drop.
function insertFilePathSorted(root: FolderNode, path: string): void {
  const parts = path.split('/').filter(Boolean);
  if (!parts.length) return;
  let node = root;
  for (let i = 0; i < parts.length - 1; i++) node = findOrCreateFolder(node, parts[i], false);
  const filename = parts[parts.length - 1];
  if (node.files.includes(filename)) return;
  const insertAt = node.files.findIndex((f) => f.localeCompare(filename) > 0);
  if (insertAt === -1) node.files.push(filename);
  else node.files.splice(insertAt, 0, filename);
}

function insertFolderPath(root: FolderNode, path: string): void {
  let node = root;
  for (const name of path.split('/').filter(Boolean)) node = findOrCreateFolder(node, name, true);
}

function collectFolderPaths(node: FolderNode, out: string[]): void {
  out.push(node.path);
  for (const folder of node.folders) collectFolderPaths(folder, out);
}

function removeFilePath(root: FolderNode, path: string): void {
  const parts = path.split('/').filter(Boolean);
  if (!parts.length) return;
  let node = root;
  for (let i = 0; i < parts.length - 1; i++) {
    const next = node.folders.find((f) => f.name === parts[i]);
    if (!next) return;
    node = next;
  }
  const idx = node.files.indexOf(parts[parts.length - 1]);
  if (idx !== -1) node.files.splice(idx, 1);
}

// Per-extension/language file icons (the repo's own code view already has
// these — Gitea's Material icon theme, modules/fileicon) aren't something
// we can replicate client-side: the name→icon rules and the icon SVGs
// themselves are Go-embedded JSON (options/fileicon/*.json), never shipped
// to the browser. Reusing them means calling the same endpoint the native
// collapsible file-tree sidebar uses (repo.TreeViewNodes) instead of
// guessing extensions ourselves. It's a per-directory endpoint (designed
// for lazy expand-on-click), so this fetches once per folder actually
// present in the tree and caches the result by full path.
type TreeViewNode = {
  entryName: string;
  entryMode: string;
  entryIcon: string;
  entryIconOpen?: string;
  fullPath: string;
};
type TreeViewResponse = {
  fileTreeNodes: TreeViewNode[];
  renderedIconPool: Record<string, string>;
};

// This editor used to mirror unsaved edits into localStorage only, and
// restore-on-open unconditionally — dropped after a stale draft silently
// outranking freshly-fetched (or freshly AI-generated) content turned out
// to be a much easier way to lose real, already-committed work than the
// crash scenario it was meant to guard against (docs/company/ai-agent.md).
// Not-yet-Saved edits are now staged server-side instead
// (company/workspace_tmp.go) — a real, authoritative "in-progress drafts"
// store, asynchronously and best-effort (never blocks typing or Save on a
// round trip), reconciled against actual current branch content exactly
// once, right when the page loads (recoverPendingEdits below), never
// silently mid-session.
//
// localStorage still has one narrow job: a write to that server store can
// itself be in flight when the tab closes (network drop, or just a slow
// connection racing a fast close) — the content for that specific,
// still-unacknowledged write is mirrored here first and only cleared once
// the server actually confirms it has it, so it isn't lost in that
// specific window. It's never treated as authoritative on its own:
// recoverPendingEdits below only ever prefers it over the server's own
// copy for the same path when its own timestamp is newer.
/* eslint-disable no-restricted-globals -- modules/user-settings.ts only wraps
   single fixed keys; this needs prefix-scoped enumeration, which it doesn't support. */
const LEGACY_DRAFT_PREFIX = 'company-workspace-draft:';
const PENDING_PREFIX = 'company-workspace-pending:';
const TMP_SAVE_DEBOUNCE_MS = 1000;

function clearLegacyDrafts(): void {
  for (const key of Object.keys(localStorage)) {
    if (key.startsWith(LEGACY_DRAFT_PREFIX)) localStorage.removeItem(key);
  }
}

type PendingWrite = {content: string, createdAt: number};

function pendingKeyPrefix(repoLink: string, branch: string): string {
  return `${PENDING_PREFIX}${repoLink}:${branch}:`;
}

function pendingKey(repoLink: string, branch: string, path: string): string {
  return `${pendingKeyPrefix(repoLink, branch)}${path}`;
}

function readPending(key: string): PendingWrite | null {
  const raw = localStorage.getItem(key);
  if (!raw) return null;
  try {
    return JSON.parse(raw) as PendingWrite;
  } catch {
    localStorage.removeItem(key);
    return null;
  }
}

export function initCompanyWorkspace() {
  registerGlobalInitFunc('initCompanyWorkspace', async (el: HTMLElement) => {
    const repoLink = el.getAttribute('data-repo-link')!;
    const branch = el.getAttribute('data-branch')!;
    const saveUrl = el.getAttribute('data-save-url')!;
    const aiUrl = el.getAttribute('data-ai-url')!;
    const tmpUrl = el.getAttribute('data-tmp-url')!;

    clearLegacyDrafts(); // one-time hygiene, see the comment above — nothing writes that prefix anymore, and this is NOT repeated on leave: pending writes below need to survive a close if the server hasn't acknowledged them yet.

    // How tall the navbar above and the footer below end up (theme,
    // window width, browser chrome) isn't something this page can know in
    // advance, so a fixed calc(100vh - Npx) in CSS always ends up either
    // leaving dead space or clipping content the moment either one's real
    // height drifts from whatever number was hardcoded. Measuring el's own
    // actual top offset at runtime and sizing to reach the viewport's
    // bottom (minus a little clearance so the footer still shows) adapts
    // to whatever's actually rendered, no guessing required.
    const VIEWPORT_BOTTOM_CLEARANCE_PX = 24;
    function sizeToViewport(): void {
      const top = el.getBoundingClientRect().top;
      const height = window.innerHeight - top - VIEWPORT_BOTTOM_CLEARANCE_PX;
      el.style.height = `${Math.max(height, 480)}px`; // 480 matches the CSS min-height fallback
    }
    sizeToViewport();
    new ResizeObserver(sizeToViewport).observe(document.documentElement);

    const treeEl = el.querySelector<HTMLElement>('.company-workspace-tree')!;
    const tabsEl = el.querySelector<HTMLElement>('.company-workspace-tabs')!;
    const panesEl = el.querySelector<HTMLElement>('.company-workspace-panes')!;
    const saveButton = el.querySelector<HTMLButtonElement>('.company-workspace-save')!;
    const deleteButton = el.querySelector<HTMLButtonElement>('.company-workspace-delete')!;
    const newFileButton = el.querySelector<HTMLButtonElement>('.company-workspace-new-file')!;
    const newFolderButton = el.querySelector<HTMLButtonElement>('.company-workspace-new-folder')!;
    const statusEl = el.querySelector<HTMLElement>('.company-workspace-status')!;

    const openTabs = new Map<string, OpenTab>();
    let activePath: string | null = null;
    // Set once initChatPanel runs, near the end of this function — declared
    // here (not there) since activateTab, defined well before that point,
    // needs to notify it of every tab switch as they happen.
    let chatHandle: ChatPanelHandle | null = null;
    // Paths deleted locally that also exist on the server — carried until
    // the next save, which is when the delete actually happens (one commit
    // with everything else, not its own separate commit).
    const pendingDeletes = new Set<string>();
    // Clicking a folder (not opening a file) also "selects" it, same idea
    // as activePath for files — whichever was clicked most recently wins
    // as the target for the next "새 파일"/"새 폴더". Cleared whenever a
    // file gets opened, so opening a file makes that file's own folder
    // authoritative again.
    let selectedFolderPath: string | null = null;
    const treeRoot: FolderNode = {name: '', path: '', folders: [], files: []};
    const collapsedFolders = new Set<string>();

    // Icon HTML per full path, filled in by fetchFolderIcons() below.
    // <use href="#svg-mfi-xxx"> in that HTML resolves against whatever
    // <svg id="svg-mfi-xxx"> defs have been merged into iconPoolEl — kept
    // outside treeEl since renderTree() wipes and rebuilds that on every
    // change.
    const iconCache = new Map<string, {icon: string, iconOpen: string}>();
    const iconPoolEl = document.createElement('div');
    iconPoolEl.className = 'tw-hidden';
    el.append(iconPoolEl);

    const encodePath = (path: string) => path.split('/').map(encodeURIComponent).join('/');

    async function fetchFolderIcons(folderPath: string): Promise<void> {
      const url = `${repoLink}/tree-view/branch/${encodeURIComponent(branch)}/${folderPath ? encodePath(folderPath) : ''}?sub_path=`;
      try {
        const resp = await GET(url);
        if (!resp.ok) return;
        const data = await resp.json() as TreeViewResponse;
        for (const [id, html] of Object.entries(data.renderedIconPool || {})) {
          if (!iconPoolEl.querySelector(`#${CSS.escape(id)}`)) iconPoolEl.insertAdjacentHTML('beforeend', html);
        }
        for (const node of data.fileTreeNodes || []) {
          iconCache.set(node.fullPath, {icon: node.entryIcon, iconOpen: node.entryIconOpen || node.entryIcon});
        }
      } catch {
        // fall back to the generic octicons already used for anything not in the cache
      }
    }

    const setStatus = (text: string) => { statusEl.textContent = text };
    const isAnyDirty = () => openTabs.values().some((tab) => tab.textarea.value !== tab.originalContent);

    // Back button, closing the tab, typing a new URL — anything that
    // navigates away without saving. Browsers ignore any custom message
    // here and show their own generic "leave site?" wording (security
    // restriction, not something we can change), but the prompt itself
    // still appears exactly when there's something unsaved.
    window.addEventListener('beforeunload', (e) => {
      if (!isAnyDirty()) return;
      e.preventDefault(); // spec-current way to trigger the browser's own leave-site prompt
    });

    function activateTab(path: string) {
      activePath = path;
      selectedFolderPath = null; // opening a file re-establishes its own folder as the target
      for (const [p, tab] of openTabs) {
        const isActive = p === path;
        tab.tabEl.classList.toggle('active', isActive);
        tab.pane.classList.toggle('tw-hidden', !isActive);
      }
      renderTree(); // updates which tree row shows the "selected" highlight
      chatHandle?.setActiveFilePath(path);
    }

    function closeTab(path: string) {
      const tab = openTabs.get(path);
      if (!tab) return;
      if (tab.textarea.value !== tab.originalContent && !window.confirm(`'${path}'의 변경사항을 버릴까요?`)) return;
      clearTimeout(tab.tmpSaveTimer);
      localStorage.removeItem(pendingKey(repoLink, branch, path)); // explicit discard — don't offer it back next time
      clearTmpEditRemote(path);
      tab.tabEl.remove();
      tab.pane.remove();
      openTabs.delete(path);
      if (activePath !== path) return;
      const next = openTabs.keys().next().value;
      if (next) activateTab(next);
      else {
        activePath = null;
        chatHandle?.setActiveFilePath(null);
      }
    }

    // Deletes the currently-open (active) file: closes its tab, drops it
    // from the tree, and — only if it actually exists on the server —
    // queues a real delete for the next save. A file that was only ever a
    // local, unsaved "새 파일" just disappears with nothing to tell the
    // server about.
    function deleteActiveFile(): void {
      if (!activePath) return;
      const path = activePath;
      const tab = openTabs.get(path);
      if (!tab) return;
      if (!window.confirm(`'${path}'을(를) 삭제할까요?`)) return;

      clearTimeout(tab.tmpSaveTimer);
      localStorage.removeItem(pendingKey(repoLink, branch, path));
      clearTmpEditRemote(path);
      tab.tabEl.remove();
      tab.pane.remove();
      openTabs.delete(path);
      removeFilePath(treeRoot, path);
      if (tab.serverPath) pendingDeletes.add(tab.serverPath);

      const next = openTabs.keys().next().value;
      if (next) activateTab(next);
      else {
        activePath = null;
        chatHandle?.setActiveFilePath(null);
        renderTree();
      }
      setStatus(tab.serverPath ? `'${path}' 삭제 예정 — 저장하면 반영됩니다.` : `'${path}'을(를) 지웠습니다.`);
    }

    deleteButton.addEventListener('click', deleteActiveFile);

    // content is the true baseline (originalContent) the dirty-check and
    // Save compare against — normally also what's shown, except when
    // initialValue is given (recoverPendingEdits below, restoring a
    // staged-but-unsaved edit): then the editor seeds from initialValue
    // instead, while content stays the real server baseline, so the
    // recovered tab correctly shows as dirty and Save sends the recovered
    // content, not a no-op.
    async function openFile(path: string, content: string, serverPath: string | null = null, initialValue?: string): Promise<void> {
      const existing = openTabs.get(path);
      if (existing) {
        activateTab(path);
        return;
      }

      const tabEl = createElementFromHTML<HTMLElement>(
        '<div class="company-workspace-tab"><span class="company-workspace-tab-name"></span><button type="button" class="company-workspace-tab-close">×</button></div>',
      );
      tabsEl.append(tabEl);

      // createCodeEditor expects its textarea to live inside a <form> — every
      // native Gitea editor page has one (the commit form); this page
      // doesn't submit anything as a form, but needs the ancestor anyway or
      // createCodeEditor's own `textarea.closest('form')!.querySelector(...)`
      // throws (modules/codeeditor/main.ts). Prevent the (never-triggered
      // by us) implicit submit just in case a future keymap adds one.
      const pane = createElementFromHTML<HTMLElement>(
        '<div class="company-workspace-pane tw-hidden"><form class="company-workspace-editor-form"><div class="editor-loading">불러오는 중…</div></form></div>',
      );
      panesEl.append(pane);
      const formEl = pane.querySelector('form')!;
      formEl.addEventListener('submit', (e) => e.preventDefault());

      const textarea = document.createElement('textarea');
      // createCodeEditor only replaces the .editor-loading placeholder with
      // the CodeMirror view — it never hides the source textarea itself.
      // The native edit page's textarea carries this same class in its
      // template (templates/repo/editor/edit.tmpl's #edit_area); ours is
      // built in JS so it has to be set here instead.
      textarea.className = 'tw-hidden';
      textarea.setAttribute('data-code-editor-config', JSON.stringify({filename: path.split('/').pop()}));

      // createCodeEditor seeds CodeMirror from `textarea.defaultValue`, not
      // `.value` (modules/codeeditor/main.ts) — defaultValue mirrors the
      // element's text content and setting `.value` alone never touches it,
      // so an existing file's fetched content rendered as an empty editor
      // until this used defaultValue too.
      textarea.defaultValue = initialValue ?? content;
      formEl.append(textarea);

      // The tab object is created before its own event handlers so those
      // handlers can close over `tab` and read `tab.path` live, instead of
      // capturing today's `path` in the closure — moveFileToFolder() below
      // renames `tab.path` in place, and a stale captured string would
      // make the tab's own click/close handlers silently target the path
      // it *used* to have.
      const tab: OpenTab = {path, serverPath, textarea, pane, tabEl, originalContent: content};
      openTabs.set(path, tab);

      tabEl.querySelector('.company-workspace-tab-name')!.textContent = tab.path;
      tabEl.addEventListener('click', (e) => {
        if ((e.target as HTMLElement).closest('.company-workspace-tab-close')) return;
        activateTab(tab.path);
      });
      tabEl.querySelector('.company-workspace-tab-close')!.addEventListener('click', (e) => {
        e.stopPropagation();
        closeTab(tab.path);
      });

      textarea.addEventListener('change', () => {
        clearTimeout(tab.tmpSaveTimer);
        tab.tmpSaveTimer = setTimeout(() => stageTmpEdit(tab), TMP_SAVE_DEBOUNCE_MS);
      });

      tab.editor = await createCodeEditor(textarea);
      activateTab(tab.path);
    }

    async function openExistingFile(path: string) {
      setStatus('불러오는 중…');
      try {
        // Gitea's native raw-content endpoint sets Cache-Control:
        // private, max-age=21600 (6h) — fine for a commit-pinned URL, but
        // this one's branch-relative: the exact same URL's real content
        // changes on every Save. Without cache: 'no-store', a browser
        // that already has this exact path cached from earlier in the
        // session (or an earlier visit) serves that stale copy straight
        // from disk cache after a Save, without even asking the server —
        // reopening a just-saved file could show what it looked like
        // before the edit. Forcing the browser to skip its cache is a
        // fix scoped to this editor's own fetches; the endpoint's own
        // caching is left alone for whatever else relies on it.
        const resp = await GET(`${repoLink}/raw/branch/${encodeURIComponent(branch)}/${encodePath(path)}`, {cache: 'no-store'});
        if (!resp.ok) throw new Error(String(resp.status));
        await openFile(path, await resp.text(), path);
        setStatus('');
      } catch {
        setStatus('');
        showErrorToast(`파일을 불러오지 못했습니다: ${path}`);
      }
    }

    // Fire-and-forget: mirrors one tab's current content to the server's
    // tmp staging area (company/workspace_tmp.go) so it survives a crash
    // or a dropped connection before the next real Save. Never awaited by
    // anything that would block typing or Save on it — the localStorage
    // write happens first and synchronously specifically so the content
    // is safe even if this exact request never completes (tab closed,
    // network drop): readPending picks it back up next visit regardless
    // of whether the POST below ever landed.
    async function stageTmpEdit(tab: OpenTab): Promise<void> {
      const createdAt = Date.now();
      const key = pendingKey(repoLink, branch, tab.path);
      localStorage.setItem(key, JSON.stringify({content: tab.textarea.value, createdAt} satisfies PendingWrite));
      try {
        const resp = await POST(tmpUrl, {data: {path: tab.path, content: tab.textarea.value, createdAt}});
        if (resp.ok) localStorage.removeItem(key); // server now durably has it — this tab's own copy of the fallback is redundant
      } catch {
        // stays in localStorage; recoverPendingEdits picks it up if the page reloads before a later write succeeds
      }
    }

    // Fire-and-forget explicit discard — closeTab/deleteActiveFile call
    // this so the server's own staged copy doesn't quietly resurrect
    // content the person just chose to throw away the next time they
    // open this branch. Not awaited: closing a tab shouldn't wait on a
    // network round trip.
    async function clearTmpEditRemote(path: string): Promise<void> {
      try {
        // createdAt here has to be able to outrank a debounced write that
        // was already in flight the moment this discard happened (same
        // reasoning as the Save handler's own clear, company/workspace.go) —
        // this browser tab's own clock, right now, always qualifies.
        await POST(tmpUrl, {data: {path, deleted: true, createdAt: Date.now()}});
      } catch {
        // best-effort — worst case it just ages out via the server's own TTL
      }
    }

    // Runs once, right after the page loads (never mid-session — the
    // whole reason the old always-on-open draft-restore caused real
    // trouble): recovers whatever wasn't Saved last time, reconciling the
    // server's own staged copy (company/workspace_tmp.go) against
    // anything still sitting in localStorage from a write that might
    // never have reached it (tab closed mid-request). Newer timestamp
    // wins per path; each winner opens as a tab seeded with the
    // recovered content while originalContent stays the real server
    // baseline, so it correctly shows as unsaved and Save sends the
    // right thing.
    async function recoverPendingEdits(): Promise<void> {
      const merged = new Map<string, PendingWrite>();
      try {
        const resp = await GET(tmpUrl);
        if (resp.ok) {
          const {entries} = await resp.json() as {entries: {path: string, content: string, createdAt: number}[]};
          for (const e of entries) merged.set(e.path, {content: e.content, createdAt: e.createdAt});
        }
      } catch {
        // server list failed — still worth checking localStorage below rather than giving up entirely
      }

      const prefix = pendingKeyPrefix(repoLink, branch);
      for (const key of Object.keys(localStorage)) {
        if (!key.startsWith(prefix)) continue;
        const local = readPending(key);
        if (!local) continue;
        const path = key.slice(prefix.length);
        const existing = merged.get(path);
        if (!existing || local.createdAt > existing.createdAt) merged.set(path, local);
      }

      if (!merged.size) return;
      setStatus(`저장하지 않은 편집 내용 ${merged.size}개를 복구하는 중…`);
      for (const [path, entry] of merged) {
        try {
          // cache: 'no-store' — see the comment on this same call in openExistingFile above.
          const resp = await GET(`${repoLink}/raw/branch/${encodeURIComponent(branch)}/${encodePath(path)}`, {cache: 'no-store'});
          const serverContent = resp.ok ? await resp.text() : ''; // not on the branch yet — recovering a file that was never saved at all
          insertFilePath(treeRoot, path, true);
          await openFile(path, serverContent, resp.ok ? path : null, entry.content);
        } catch {
          // leave this one's server/localStorage entry alone — picked up again next visit
        }
      }
      renderTree();
      setStatus('');
    }

    // Drag-and-drop move (files only — dragging a whole folder isn't
    // supported yet). Moving only ever changes tab.path; tab.serverPath
    // stays wherever the content actually lives until a save actually
    // renames it there — see the OpenTab type and the save handler below.
    // An unopened file gets opened first (fetches its real content) so it
    // has somewhere to hold that pending rename until save.
    async function moveFileToFolder(path: string, targetFolder: string): Promise<void> {
      const basename = path.split('/').pop()!;
      const newPath = targetFolder ? `${targetFolder}/${basename}` : basename;
      if (newPath === path) return;
      if (openTabs.has(newPath)) {
        showErrorToast(`이미 '${newPath}' 파일이 있습니다.`);
        return;
      }

      if (!openTabs.has(path)) {
        await openExistingFile(path);
        if (!openTabs.has(path)) return; // failed to load — openExistingFile already reported it
      }
      const tab = openTabs.get(path)!;

      openTabs.delete(path);
      tab.path = newPath;
      tab.tabEl.querySelector('.company-workspace-tab-name')!.textContent = newPath;
      openTabs.set(newPath, tab);
      if (activePath === path) activePath = newPath;

      removeFilePath(treeRoot, path);
      insertFilePathSorted(treeRoot, newPath);
      // iconCache is keyed by full path (fetched once per folder from
      // Gitea's own tree-view endpoint) — without carrying the entry over,
      // the moved file's per-type icon reverts to the generic fallback
      // the moment it's at a path that was never actually fetched.
      const icon = iconCache.get(path);
      if (icon) iconCache.set(newPath, icon);
      activateTab(newPath);
      setStatus(`'${path}' → '${newPath}'로 이동했습니다. 저장하면 반영됩니다.`);
    }

    let dragSourcePath: string | null = null;

    function wireDropTarget(el: HTMLElement, targetFolder: string): void {
      el.addEventListener('dragover', (e) => {
        if (!dragSourcePath) return;
        e.preventDefault();
        e.stopPropagation();
        el.classList.add('drop-target');
      });
      el.addEventListener('dragleave', () => el.classList.remove('drop-target'));
      el.addEventListener('drop', (e) => {
        e.preventDefault();
        e.stopPropagation();
        el.classList.remove('drop-target');
        const source = dragSourcePath;
        dragSourcePath = null;
        if (source) moveFileToFolder(source, targetFolder);
      });
    }

    // Set while the "새 파일"/"새 폴더" inline input row is showing — an
    // in-place text field instead of window.prompt(), VSCode-style. `parent`
    // is which folder it appears (and creates) in: the currently active
    // file's own folder if one's open, root otherwise. Path input still
    // works here too (typing "sub/x.txt" nests further under `parent`).
    let pendingNewItem: {kind: 'file' | 'folder', parent: string} | null = null;

    // The active tab's own folder — "src/a/b.txt" -> "src/a", a root file -> "".
    function activeFileParent(): string {
      if (!activePath) return '';
      const idx = activePath.lastIndexOf('/');
      return idx === -1 ? '' : activePath.slice(0, idx);
    }

    // A clicked folder wins over an open file's own folder — whichever
    // happened more recently, since activateTab() and the folder click
    // handler each clear the other's selection.
    function newItemTargetParent(): string {
      return selectedFolderPath ?? activeFileParent();
    }

    function commitNewItem(kind: 'file' | 'folder', parent: string, rawValue: string): void {
      pendingNewItem = null;
      const value = rawValue.trim();
      if (!value) {
        renderTree();
        return;
      }
      const fullValue = parent ? `${parent}/${value}` : value;
      if (kind === 'file') {
        if (openTabs.has(fullValue)) {
          renderTree();
          activateTab(fullValue);
          return;
        }
        insertFilePath(treeRoot, fullValue, true);
        renderTree();
        openFile(fullValue, '');
      } else {
        insertFolderPath(treeRoot, fullValue);
        collapsedFolders.delete(fullValue);
        renderTree();
      }
    }

    function buildNewItemRow(kind: 'file' | 'folder', parent: string, depth: number): HTMLElement {
      const iconSvg = kind === 'file' ? svg('octicon-file', 14) : svg('octicon-file-directory-fill', 14);
      const placeholder = kind === 'file' ? '파일 이름 (예: notes.txt, sub/notes.txt)' : '폴더 이름';
      const row = createElementFromHTML<HTMLElement>(
        `<div class="company-workspace-tree-row company-workspace-tree-new-item" style="padding-left:${depth * 16 + (kind === 'file' ? 18 : 0)}px">` +
        `${iconSvg}<input type="text" class="company-workspace-tree-new-input" placeholder="${placeholder}"></div>`,
      );
      const input = row.querySelector('input')!;
      let committed = false;
      input.addEventListener('keydown', (e) => {
        if (e.key === 'Enter') {
          e.preventDefault();
          committed = true;
          commitNewItem(kind, parent, input.value);
        } else if (e.key === 'Escape') {
          e.preventDefault();
          committed = true;
          pendingNewItem = null;
          renderTree();
        }
      });
      // Clicking away commits too (empty input just cancels, same as Escape)
      // — but skip it if Enter/Escape already handled this row, since that
      // already re-rendered and this input element no longer represents
      // pendingNewItem's row.
      input.addEventListener('blur', () => {
        if (committed) return;
        committed = true;
        commitNewItem(kind, parent, input.value);
      });
      return row;
    }

    function renderTree(): void {
      if (pendingNewItem) collapsedFolders.delete(pendingNewItem.parent); // make sure its folder is open so the input is visible
      renderFolderContents(treeRoot, treeEl, 0);
    }

    function renderFolderContents(node: FolderNode, container: HTMLElement, depth: number): void {
      container.replaceChildren();
      if (pendingNewItem?.parent === node.path) {
        const row = buildNewItemRow(pendingNewItem.kind, pendingNewItem.parent, depth);
        container.append(row);
        row.querySelector('input')!.focus();
      }
      for (const folder of node.folders) {
        const collapsed = collapsedFolders.has(folder.path);
        const chevronSvg = svg(collapsed ? 'octicon-chevron-right' : 'octicon-chevron-down', 12);
        const cachedFolderIcon = iconCache.get(folder.path);
        const folderSvg = cachedFolderIcon ?
          (collapsed ? cachedFolderIcon.icon : cachedFolderIcon.iconOpen) :
          svg(collapsed ? 'octicon-file-directory-fill' : 'octicon-file-directory-open-fill', 14);
        const row = createElementFromHTML<HTMLElement>(
          `<div class="company-workspace-tree-row company-workspace-tree-folder" style="padding-left:${depth * 16}px">` +
          `<span class="company-workspace-tree-chevron">${chevronSvg}</span>${folderSvg}` +
          `<span class="company-workspace-tree-name"></span></div>`,
        );
        row.querySelector('.company-workspace-tree-name')!.textContent = folder.name;
        row.classList.toggle('selected', folder.path === selectedFolderPath);
        row.addEventListener('click', () => {
          if (collapsed) collapsedFolders.delete(folder.path);
          else collapsedFolders.add(folder.path);
          selectedFolderPath = folder.path;
          renderTree();
        });
        wireDropTarget(row, folder.path);
        container.append(row);
        if (!collapsed) {
          const childContainer = createElementFromHTML<HTMLElement>('<div class="company-workspace-tree-children"></div>');
          wireDropTarget(childContainer, folder.path); // dropping on empty space at this level = this folder too
          container.append(childContainer);
          renderFolderContents(folder, childContainer, depth + 1);
        }
      }
      for (const file of node.files) {
        const fullPath = node.path ? `${node.path}/${file}` : file;
        const fileSvg = iconCache.get(fullPath)?.icon ?? svg('octicon-file', 14);
        const row = createElementFromHTML<HTMLElement>(
          `<div class="company-workspace-tree-row company-workspace-tree-item" style="padding-left:${depth * 16 + 18}px">` +
          `${fileSvg}<span class="company-workspace-tree-name"></span></div>`,
        );
        row.querySelector('.company-workspace-tree-name')!.textContent = file;
        row.classList.toggle('selected', fullPath === activePath);
        // openExistingFile always fetches from the server — right for a
        // path that's never been opened this session, wrong for one that's
        // already an open tab: an AI-proposed or "새 파일" tab has nothing
        // on the server yet, so that fetch 404s and the click silently
        // fails to bring the (already-open, already has content) tab back.
        row.addEventListener('click', () => {
          if (openTabs.has(fullPath)) activateTab(fullPath);
          else openExistingFile(fullPath);
        });
        row.draggable = true;
        row.addEventListener('dragstart', (e) => {
          dragSourcePath = fullPath;
          e.dataTransfer!.effectAllowed = 'move';
          e.dataTransfer!.setData('text/plain', fullPath);
        });
        row.addEventListener('dragend', () => { dragSourcePath = null });
        wireDropTarget(row, node.path); // dropping directly on a file = same folder as that file
        container.append(row);
      }
    }
    wireDropTarget(treeEl, ''); // empty space below everything = root
    // Clicking empty space (not a row) cancels an explicit folder
    // selection, so "새 파일" falls back to the active file's own folder
    // (or root) instead of wherever was last clicked.
    treeEl.addEventListener('click', (e) => {
      if (e.target !== treeEl || selectedFolderPath === null) return;
      selectedFolderPath = null;
      renderTree();
    });

    newFileButton.addEventListener('click', () => {
      pendingNewItem = {kind: 'file', parent: newItemTargetParent()};
      renderTree();
    });

    // Applies one AI-proposed edit (company/workspace_ai.go's {"type":"edit"}
    // event, relayed by company-ai-chat.ts) — same "still requires a Save"
    // rule as every other change in this editor, see docs/company/ai-agent.md.
    // An already-open tab just gets its content replaced (still dirty until
    // saved, same as if the person had typed it); a path with no open tab
    // is opened fresh, exactly like clicking a new tree row would.
    function applyAIEdit(path: string, content: string): void {
      const existing = openTabs.get(path);
      if (existing) {
        // CodeMirror never notices an external write to textarea.value —
        // it only writes to the textarea itself (via its own updateListener,
        // modules/codeeditor/main.ts), never reads from it after the initial
        // load. Setting .value directly here left the on-screen editor
        // showing the old content while the hidden textarea silently had
        // the new one — the AI-proposed edit "worked" but was invisible.
        // Dispatching the replacement into the live view instead goes
        // through that same updateListener, which syncs the textarea for
        // free — so nothing else here needs to change.
        if (existing.editor) {
          const {view} = existing.editor;
          view.dispatch({changes: {from: 0, to: view.state.doc.length, insert: content}});
        } else {
          // Editor hasn't finished mounting yet (openFile is still awaiting
          // createCodeEditor) — falling back to the textarea is safe here
          // since createCodeEditor itself reads defaultValue/value once at
          // mount, so this still ends up on screen once it finishes loading.
          existing.textarea.value = content;
        }
        activateTab(path);
        return;
      }
      insertFilePath(treeRoot, path, true);
      renderTree();
      openFile(path, content);
    }

    const aiChatEl = el.querySelector<HTMLElement>('.company-workspace-ai-chat');
    if (aiChatEl) {
      chatHandle = initChatPanel(aiChatEl, {
        sendUrl: aiUrl,
        onEdit: applyAIEdit,
        emptyStateText: '파일을 읽고 수정을 제안해드려요.<br>저장은 직접 눌러야 반영됩니다.',
        // Every open tab's live content — including whatever's not saved
        // yet — not just the active one: read_file needs to see an
        // unsaved edit in ANY open tab, not only whichever happened to be
        // on screen when the message was sent (company/workspace_ai.go).
        getContext: () => ({
          activePath,
          openFiles: openTabs.values().map((tab) => ({path: tab.path, content: tab.textarea.value})).toArray(),
        }),
      });
      if (activePath) chatHandle.setActiveFilePath(activePath);
    }

    newFolderButton.addEventListener('click', () => {
      pendingNewItem = {kind: 'folder', parent: newItemTargetParent()};
      renderTree();
    });

    saveButton.addEventListener('click', async () => {
      // A tab needs saving if its content changed OR it was dragged into
      // a different folder (path !== serverPath) with no content change.
      const changedTabs = openTabs.values()
        .filter((tab) => tab.textarea.value !== tab.originalContent || tab.serverPath !== tab.path)
        .toArray();
      const files = changedTabs.map((tab) => ({
        path: tab.path,
        content: tab.textarea.value,
        ...tab.serverPath && tab.serverPath !== tab.path && {fromPath: tab.serverPath},
      }));
      const deletes = [...pendingDeletes].map((path) => ({path, deleted: true}));
      if (!files.length && !deletes.length) {
        setStatus('변경된 내용이 없습니다.');
        return;
      }
      saveButton.disabled = true;
      setStatus('저장 중…');
      try {
        const resp = await POST(saveUrl, {data: {files: [...files, ...deletes]}});
        if (!resp.ok) throw new Error(String(resp.status));
        // This Save just became the authoritative content for every one of
        // these paths — the server already clears its own staged copy as
        // part of handling it (company/workspace.go's WorkspaceSave); the
        // pending debounce timer and this tab's own localStorage fallback
        // are cleared here for the same reason (see the OpenTab type on
        // tmpSaveTimer): a keystroke right before clicking Save otherwise
        // left both still able to re-stage now-superseded content moments
        // later.
        for (const tab of changedTabs) {
          clearTimeout(tab.tmpSaveTimer);
          localStorage.removeItem(pendingKey(repoLink, branch, tab.path));
          tab.originalContent = tab.textarea.value;
          tab.serverPath = tab.path;
        }
        pendingDeletes.clear();
        setStatus('저장했습니다.');
      } catch {
        setStatus('');
        showErrorToast('저장에 실패했습니다.');
      } finally {
        saveButton.disabled = false;
      }
    });

    try {
      const resp = await GET(`${repoLink}/tree-list/branch/${encodeURIComponent(branch)}`);
      const paths: string[] = await resp.json();
      for (const path of paths) insertFilePath(treeRoot, path, false); // server order, appended below any new entries
      renderTree(); // paint the structure immediately with generic icons

      const folderPaths: string[] = [];
      collectFolderPaths(treeRoot, folderPaths);
      await Promise.all(folderPaths.map(fetchFolderIcons));
      renderTree(); // repaint once the real per-file-type icons are in
    } catch {
      showErrorToast('파일 목록을 불러오지 못했습니다.');
    }

    await recoverPendingEdits();
  });
}
