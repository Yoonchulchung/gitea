import {registerGlobalInitFunc} from '../modules/observer.ts';
import {createCodeEditor} from '../modules/codeeditor/main.ts';
import type {CodemirrorEditor} from '../modules/codeeditor/main.ts';
import {createElementFromHTML} from '../utils/dom.ts';
import {showErrorToast} from '../modules/toast.ts';
import {GET, POST} from '../modules/fetch.ts';
import {svg} from '../svg.ts';
import {initChatPanel} from './company-ai-chat.ts';
import type {ChatPanelHandle} from './company-ai-chat.ts';
import {formatDatetime} from '../utils/time.ts';
import {attachConflictUI, buildConflictText, conflictMarkerPattern} from './company-conflict.ts';

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
  // Blob SHA this tab's content was opened from (the raw-content
  // endpoint's own ETag — a git blob ID), sent with Save so the server
  // can tell whether someone else saved a newer version of this exact
  // file in the meantime (company/workspace.go's ErrSHADoesNotMatch
  // handling). undefined for a file that never existed before this tab —
  // nothing to compare against, and the server already treats a missing
  // SHA as "no check needed" for a brand-new upload.
  baseSha?: string;
};

type WorkspaceConflict = {
  path: string;
  serverContent: string;
  serverSha: string;
  serverMessage: string;
  serverAuthor: string;
  serverDate: number; // unix seconds
  baseContent?: string; // common ancestor for 3-way merging — see company/workspace.go
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

// removed (server-sourced only — see stageTmpDelete/recoverPendingEdits)
// marks this path as a pending file deletion rather than staged content;
// content/baseSha are meaningless when it's set.
type PendingWrite = {content: string, createdAt: number, baseSha?: string, removed?: boolean};

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

// Falls back to '' instead of null — a missing data-i18n-* attribute
// (a stale page from before one was added, say) previously meant
// `window.confirm(null)` displaying the literal word "null" as the
// dialog's whole message, which is worse than just showing nothing.
function attr(el: Element, name: string): string {
  return el.getAttribute(name) ?? '';
}

export function initCompanyWorkspace() {
  registerGlobalInitFunc('initCompanyWorkspace', async (el: HTMLElement) => {
    const repoLink = el.getAttribute('data-repo-link')!;
    const branch = el.getAttribute('data-branch')!;
    const saveUrl = el.getAttribute('data-save-url')!;
    const aiUrl = el.getAttribute('data-ai-url')!;
    const tmpUrl = el.getAttribute('data-tmp-url')!;
    const foldersUrl = el.getAttribute('data-folders-url')!;
    const iconUrl = el.getAttribute('data-icon-url')!;

    // Every user-facing string below comes from here, not a hardcoded
    // literal — so this page follows whatever language it's rendered in
    // (custom/templates/company/workspace.tmpl), not just Korean. Each
    // still has its own "%s"/"%d" placeholder(s) baked in server-side by
    // ctx.Locale.Tr, substituted client-side with a plain .replace().
    const i18n = {
      confirmDiscardTab: attr(el, 'data-i18n-confirm-discard-tab'),
      confirmDeleteFile: attr(el, 'data-i18n-confirm-delete-file'),
      statusDeletePending: attr(el, 'data-i18n-status-delete-pending'),
      statusDeleted: attr(el, 'data-i18n-status-deleted'),
      loading: attr(el, 'data-i18n-loading'),
      errorLoadFile: attr(el, 'data-i18n-error-load-file'),
      statusRecovering: attr(el, 'data-i18n-status-recovering'),
      errorFileExists: attr(el, 'data-i18n-error-file-exists'),
      errorBinaryFile: attr(el, 'data-i18n-error-binary-file'),
      statusMoved: attr(el, 'data-i18n-status-moved'),
      newFilePlaceholder: attr(el, 'data-i18n-new-file-placeholder'),
      newFolderPlaceholder: attr(el, 'data-i18n-new-folder-placeholder'),
      statusNoChanges: attr(el, 'data-i18n-status-no-changes'),
      statusSaving: attr(el, 'data-i18n-status-saving'),
      statusSaved: attr(el, 'data-i18n-status-saved'),
      errorSaveFailed: attr(el, 'data-i18n-error-save-failed'),
      folderClosed: attr(el, 'data-i18n-folder-closed'),
      folderOpen: attr(el, 'data-i18n-folder-open'),
      deleteFile: attr(el, 'data-i18n-delete-file'),
      conflictYours: attr(el, 'data-i18n-conflict-yours'),
      conflictTheirs: attr(el, 'data-i18n-conflict-theirs'),
      conflictTheirsLatest: attr(el, 'data-i18n-conflict-theirs-latest'),
      statusConflict: attr(el, 'data-i18n-status-conflict'),
      conflictUseYours: attr(el, 'data-i18n-conflict-use-yours'),
      conflictUseTheirs: attr(el, 'data-i18n-conflict-use-theirs'),
      conflictUseBoth: attr(el, 'data-i18n-conflict-use-both'),
      conflictUnresolved: attr(el, 'data-i18n-conflict-unresolved'),
      statusAutoMerged: attr(el, 'data-i18n-status-auto-merged'),
    };

    clearLegacyDrafts(); // one-time hygiene, see the comment above — nothing writes that prefix anymore, and this is NOT repeated on leave: pending writes below need to survive a close if the server hasn't acknowledged them yet.

    // How tall the navbar above and the footer below end up (theme,
    // window width, browser chrome) isn't something this page can know in
    // advance, so a fixed calc(100vh - Npx) in CSS always ends up either
    // leaving dead space or clipping content the moment either one's real
    // height drifts from whatever number was hardcoded. Measuring el's own
    // actual top offset, plus the real rendered height of .full.height's
    // own bottom padding (--page-space-bottom) and the site footer below
    // it, adapts to whatever's actually rendered — a flat guess here
    // previously undercounted that space and made the whole page scroll.
    const FALLBACK_CLEARANCE_PX = 24; // only used if .page-footer/.full.height aren't found
    function sizeToViewport(): void {
      const top = el.getBoundingClientRect().top;
      const footerEl = document.querySelector<HTMLElement>('.page-footer');
      const fullHeightEl = el.closest<HTMLElement>('.full.height');
      const footerHeight = footerEl?.getBoundingClientRect().height ?? 0;
      const pageSpaceBottom = fullHeightEl ? (Number.parseFloat(getComputedStyle(fullHeightEl).paddingBottom) || 0) : 0;
      const clearance = (footerHeight + pageSpaceBottom) || FALLBACK_CLEARANCE_PX;
      const height = window.innerHeight - top - clearance;
      el.style.height = `${Math.max(height, 480)}px`; // 480 matches the CSS min-height fallback
    }
    sizeToViewport();
    new ResizeObserver(sizeToViewport).observe(document.documentElement);

    const treeEl = el.querySelector<HTMLElement>('.company-workspace-tree')!;
    const tabsEl = el.querySelector<HTMLElement>('.company-workspace-tabs')!;
    const panesEl = el.querySelector<HTMLElement>('.company-workspace-panes')!;
    const saveButton = el.querySelector<HTMLButtonElement>('.company-workspace-save')!;
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
    // Tracks which folders are explicitly opened, not which are collapsed —
    // an empty Set then means every folder starts collapsed by default (no
    // separate "seed every known path as collapsed" step needed, since
    // "not in this Set" already means that).
    const expandedFolders = new Set<string>();

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

    // The raw-content endpoint's ETag is a git blob SHA — read as this
    // tab's baseline for the conflict check on Save (see OpenTab.baseSha).
    // Quoted per the HTTP spec ("abc123"), stripped here since the server
    // side wants the bare SHA to compare against ChangeRepoFile.SHA.
    const readETag = (resp: Response): string | undefined => resp.headers.get('etag')?.replaceAll('"', '') ?? undefined;

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

    // Same idea as fetchFolderIcons, but for a path that isn't in the repo
    // yet at all (a brand-new tab: New File, an AI-created file, a dropped
    // file) — tree-view only knows about paths actually in the git tree,
    // so it has nothing to match a new one against. company/workspace_icon.go
    // reuses the same server-side icon-matching logic fed a synthetic
    // entry instead, keyed by filename alone.
    async function fetchFileIcon(path: string): Promise<void> {
      if (iconCache.has(path)) return;
      try {
        const resp = await GET(`${iconUrl}?name=${encodeURIComponent(path.split('/').pop()!)}`);
        if (!resp.ok) return;
        const data = await resp.json() as {icon: string, renderedIconPool: Record<string, string>};
        for (const [id, html] of Object.entries(data.renderedIconPool || {})) {
          if (!iconPoolEl.querySelector(`#${CSS.escape(id)}`)) iconPoolEl.insertAdjacentHTML('beforeend', html);
        }
        iconCache.set(path, {icon: data.icon, iconOpen: data.icon});
        renderTree();
      } catch {
        // fall back to the generic octicon already used for anything not in the cache
      }
    }

    const setStatus = (text: string) => {
      statusEl.textContent = text;
      statusEl.title = text; // the status line ellipsizes when space is tight — hover still shows all of it
    };

    // Fired by company-conflict.ts when its buttons just removed the last
    // conflict block in some tab — the "resolve the conflict below" status
    // line would otherwise keep announcing a conflict that's gone.
    el.addEventListener('company-conflict-resolved', () => setStatus(''));
    // Same "needs saving" definition the Save button itself uses below —
    // not just changed content: a brand-new file (serverPath still null)
    // or one dragged into a different folder is unsaved work too, even
    // with its content untouched (an empty new file is still a new
    // file), and so is a pending delete that hasn't been saved yet.
    const isAnyDirty = () => pendingDeletes.size > 0 ||
      openTabs.values().some((tab) => tab.textarea.value !== tab.originalContent || tab.serverPath !== tab.path);

    // Set right before intentionally letting a navigation through after
    // our own confirm below already asked once — without this, the
    // beforeunload listener right after fires its own native "leave
    // site?" prompt for the exact same navigation a moment later, so
    // confirming once still meant answering the question twice.
    let leavingConfirmed = false;

    // Back button, closing the tab, typing a new URL — anything that
    // navigates away without saving. Browsers ignore any custom message
    // here and show their own generic "leave site?" wording (security
    // restriction, not something we can change), but the prompt itself
    // still appears exactly when there's something unsaved.
    window.addEventListener('beforeunload', (e) => {
      if (!isAnyDirty() || leavingConfirmed) return;
      e.preventDefault(); // spec-current way to trigger the browser's own leave-site prompt
    });

    // Any plain link click that navigates away — the breadcrumb's own
    // "← {repo}" link, but just as much the native navbar's logo/
    // notifications/"Deploy Requests"/etc, none of which are ours to wire
    // up individually — is exactly the same case beforeunload above
    // already covers, but worth this page's own explicit, translated
    // confirm instead of leaning on the browser's fixed, unlocalizable
    // wording. Delegated on document (not each link) specifically to
    // reach that native navbar markup without touching it.
    const leaveConfirmMessage = attr(el, 'data-i18n-confirm-leave-unsaved');
    document.addEventListener('click', (e) => {
      if (!isAnyDirty() || e.defaultPrevented) return;
      if (e.button !== 0 || e.ctrlKey || e.metaKey || e.shiftKey || e.altKey) return; // let modifier/middle clicks (new tab) through untouched
      const link = (e.target as HTMLElement).closest<HTMLAnchorElement>('a[href]');
      if (!link || link.target === '_blank' || link.hasAttribute('download')) return;
      const href = link.getAttribute('href')!;
      if (href.startsWith('#') || !link.protocol.startsWith('http')) return; // same-page or non-navigating scheme (mailto:, javascript:) — not a real navigation
      if (window.confirm(leaveConfirmMessage)) {
        leavingConfirmed = true; // let beforeunload stand down for this navigation
      } else {
        e.preventDefault();
      }
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
      if (tab.textarea.value !== tab.originalContent && !window.confirm(i18n.confirmDiscardTab.replace('%s', path))) return;
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

    // Deletes any file by path, open or not: closes its tab if it has one,
    // drops it from the tree, and — only if it actually exists on the
    // server — queues a real delete for the next save. A file that was
    // only ever a local, unsaved "새 파일" just disappears with nothing to
    // tell the server about. Called from a tree row's own hover-delete
    // button (renderFolderContents below) as much as from an open tab —
    // deleting doesn't require opening it first.
    function deleteFile(path: string): void {
      if (!window.confirm(i18n.confirmDeleteFile.replace('%s', path))) return;

      const tab = openTabs.get(path);
      let hadServerPath = false;
      if (tab) {
        clearTimeout(tab.tmpSaveTimer);
        localStorage.removeItem(pendingKey(repoLink, branch, path));
        clearTmpEditRemote(path);
        tab.tabEl.remove();
        tab.pane.remove();
        openTabs.delete(path);
        if (tab.serverPath) {
          pendingDeletes.add(tab.serverPath);
          stageTmpDelete(tab.serverPath);
          hadServerPath = true;
        }
      } else {
        // Never opened this session — its tree path is its real server
        // path (an unopened, unsaved file has no tree row to begin with).
        pendingDeletes.add(path);
        stageTmpDelete(path);
        hadServerPath = true;
      }
      removeFilePath(treeRoot, path);

      if (activePath === path) {
        const next = openTabs.keys().next().value;
        if (next) activateTab(next);
        else {
          activePath = null;
          chatHandle?.setActiveFilePath(null);
        }
      }
      renderTree();
      setStatus(hadServerPath ? i18n.statusDeletePending.replace('%s', path) : i18n.statusDeleted.replace('%s', path));
    }

    // content is the true baseline (originalContent) the dirty-check and
    // Save compare against — normally also what's shown, except when
    // initialValue is given (recoverPendingEdits below, restoring a
    // staged-but-unsaved edit): then the editor seeds from initialValue
    // instead, while content stays the real server baseline, so the
    // recovered tab correctly shows as dirty and Save sends the recovered
    // content, not a no-op.
    async function openFile(path: string, content: string, serverPath: string | null = null, initialValue?: string, baseSha?: string): Promise<void> {
      const existing = openTabs.get(path);
      if (existing) {
        activateTab(path);
        return;
      }

      // A real serverPath means this path already exists in the repo —
      // its icon comes from fetchFolderIcons the normal way, once the
      // tree loads/expands to it. Only a brand-new file (New File, AI,
      // a dropped file — serverPath still null) has nothing for that to
      // match, hence its own lookup.
      if (serverPath === null) fetchFileIcon(path);

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
        `<div class="company-workspace-pane tw-hidden"><form class="company-workspace-editor-form"><div class="editor-loading">${i18n.loading}</div></form></div>`,
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
      const tab: OpenTab = {path, serverPath, textarea, pane, tabEl, originalContent: content, baseSha};
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
      // Every tab gets the conflict-resolution UI (company-conflict.ts) —
      // it renders nothing until conflict markers actually appear in the
      // document (handleConflict / recoverPendingEdits put them there).
      attachConflictUI(tab.editor.view, {
        useYours: i18n.conflictUseYours,
        useTheirs: i18n.conflictUseTheirs,
        useBoth: i18n.conflictUseBoth,
      });
      activateTab(tab.path);
    }

    async function openExistingFile(path: string) {
      setStatus(i18n.loading);
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
        await openFile(path, await resp.text(), path, undefined, readETag(resp));
        setStatus('');
      } catch {
        setStatus('');
        showErrorToast(i18n.errorLoadFile.replace('%s', path));
      }
    }

    // The synchronous half of stageTmpEdit below, split out so callers that
    // can't await a tab's own mount (applyAIEdit, for a brand-new file —
    // see there) can still guarantee the localStorage copy lands the
    // instant content exists, not only once some later async step settles.
    function writePendingLocal(path: string, content: string, createdAt: number, baseSha?: string): string {
      const key = pendingKey(repoLink, branch, path);
      try {
        localStorage.setItem(key, JSON.stringify({content, createdAt, baseSha} satisfies PendingWrite));
      } catch {
        // Quota exceeded (or storage disabled entirely) — this mirror is
        // only ever a narrow safety net for a write still in flight to the
        // server (see the comment above LEGACY_DRAFT_PREFIX), never the
        // edit's only copy. Losing it just narrows that one race window
        // back to what it was before this mirror existed; it must not
        // crash the caller (stageTmpEdit's own server POST, or a brand-new
        // dropped file's tab, still goes ahead regardless).
      }
      return key;
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
      const key = writePendingLocal(tab.path, tab.textarea.value, createdAt, tab.baseSha);
      try {
        const resp = await POST(tmpUrl, {data: {path: tab.path, content: tab.textarea.value, createdAt, baseSha: tab.baseSha}});
        if (resp.ok) localStorage.removeItem(key); // server now durably has it — this tab's own copy of the fallback is redundant
      } catch {
        // stays in localStorage; recoverPendingEdits picks it up if the page reloads before a later write succeeds
      }
    }

    // For a brand-new file created without a keystroke (drop, New File,
    // AI edit): opens its tab, then stages it the moment the tab exists —
    // no 'change' event will ever fire for it, so waiting on one would
    // mean a refresh loses it. Callers deliberately don't await this
    // (their own synchronous writePendingLocal already made the content
    // safe); the tab lookup re-checks because openFile can bail.
    async function openFileAndStage(path: string, content: string): Promise<void> {
      await openFile(path, content);
      const tab = openTabs.get(path);
      if (tab) stageTmpEdit(tab);
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

    // Stages path as a pending file deletion server-side (workspaceTmpEntry.Removed,
    // company/workspace_tmp.go) — without this, pendingDeletes only ever
    // lived in this tab's own memory, and a refresh before clicking Save
    // silently undid the delete. Called from every path that adds to
    // pendingDeletes below, not just deleteFile's own tab-was-open branch —
    // an AI-proposed delete_file (applyAIDelete) needs the exact same
    // protection, and unlike a person clicking the confirm-gated hover-X
    // button, nothing about the AI flow makes an immediate Save likely to
    // follow before the person might reload.
    async function stageTmpDelete(path: string): Promise<void> {
      try {
        await POST(tmpUrl, {data: {path, removed: true, createdAt: Date.now()}});
      } catch {
        // best-effort — worst case a refresh before Save loses the pending delete, same risk as before this existed
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
          const {entries} = await resp.json() as {entries: {path: string, content: string, createdAt: number, baseSha?: string, removed?: boolean}[]};
          for (const e of entries) merged.set(e.path, {content: e.content, createdAt: e.createdAt, baseSha: e.baseSha, removed: e.removed});
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
      setStatus(i18n.statusRecovering.replace('%s', String(merged.size))); // "%s" not "%d" — see the comment on the same pattern in company-ai-chat.ts
      let firstConflictPath: string | null = null;
      for (const [path, entry] of merged) {
        try {
          if (entry.removed) {
            // A pending deletion, not staged content — same tree/pendingDeletes
            // bookkeeping deleteFile/applyAIDelete do, minus re-staging (this
            // entry IS that staging, already on disk) and minus the confirm
            // dialog (already confirmed once, before the refresh that brought
            // us here). Nothing to open, so this path skips the rest of the
            // loop body entirely.
            pendingDeletes.add(path);
            removeFilePath(treeRoot, path);
            continue;
          }
          // cache: 'no-store' — see the comment on this same call in openExistingFile above.
          const resp = await GET(`${repoLink}/raw/branch/${encodeURIComponent(branch)}/${encodePath(path)}`, {cache: 'no-store'});
          const serverContent = resp.ok ? await resp.text() : ''; // not on the branch yet — recovering a file that was never saved at all
          const freshSha = resp.ok ? readETag(resp) : undefined;
          insertFilePath(treeRoot, path, true);
          // The draft remembers which blob SHA it started from (staged
          // alongside the content — see stageTmpEdit). If the branch has
          // a different blob now, someone else saved this file while the
          // draft sat unsaved through a refresh — the exact "A refreshed
          // at 10:03 after B saved at 10:02" case. Surface it right here
          // as git conflict markers instead of restoring the stale draft
          // as if nothing happened, which would make the next Save
          // silently overwrite that person's change (tab.baseSha is set
          // to the fresh blob below, so it couldn't 409 on its own).
          const conflicted = entry.baseSha && freshSha && entry.baseSha !== freshSha && entry.content !== serverContent;
          // No common ancestor is available here (only its SHA was staged),
          // so this is a 2-way merge: common lines stay plain, each
          // genuinely differing run gets its own marker block.
          const seed = conflicted ? buildConflictText(null, entry.content, serverContent, i18n.conflictYours, i18n.conflictTheirsLatest).text : entry.content;
          await openFile(path, serverContent, resp.ok ? path : null, seed, freshSha);
          if (conflicted) firstConflictPath ??= path;
        } catch {
          // leave this one's server/localStorage entry alone — picked up again next visit
        }
      }
      renderTree();
      if (firstConflictPath) {
        const message = i18n.statusConflict.replace('%s', firstConflictPath);
        setStatus(message);
        showErrorToast(message);
      } else {
        setStatus('');
      }
    }

    // Shared core of every path rename, however it's triggered (drag-and-drop
    // move below, or an AI-proposed rename_file — see applyAIRename further
    // down): retargets an open tab's own path, or opens the file first if it
    // wasn't already — same "still needs Save" rule as any other pending
    // change (tab.serverPath keeps the old path until a real Save actually
    // renames it there, see the OpenTab type). Returns false (and reports
    // its own error) on a name collision or a failed open — callers that
    // want to do something more (activate the tab, show a status line) only
    // do it once this comes back true.
    async function applyPathRename(path: string, newPath: string): Promise<boolean> {
      if (newPath === path) return true;
      if (openTabs.has(newPath)) {
        showErrorToast(i18n.errorFileExists.replace('%s', newPath));
        return false;
      }

      if (!openTabs.has(path)) {
        await openExistingFile(path);
        if (!openTabs.has(path)) return false; // failed to load — openExistingFile already reported it
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
      return true;
    }

    // Drag-and-drop move (files only — dragging a whole folder isn't
    // supported yet) — same folder, new basename kept the same.
    async function moveFileToFolder(path: string, targetFolder: string): Promise<void> {
      const basename = path.split('/').pop()!;
      const newPath = targetFolder ? `${targetFolder}/${basename}` : basename;
      if (!(await applyPathRename(path, newPath))) return;
      activateTab(newPath);
      setStatus(i18n.statusMoved.replace('%s', path).replace('%s', newPath));
    }

    let dragSourcePath: string | null = null;

    // A file dragged in from outside the browser (Finder/Explorer) carries
    // no dragSourcePath (that's only set by our own tree rows' dragstart,
    // above) but does show up in e.dataTransfer.files — checked for on
    // dragover too, not just drop, so the drop-target highlight (and
    // dropEffect, without which some browsers show a "forbidden" cursor)
    // appears while dragging over, the same as an internal move.
    function hasExternalFiles(e: DragEvent): boolean {
      return !dragSourcePath && (e.dataTransfer?.types.includes('Files') ?? false);
    }

    // Reads every dropped file's content and opens/stages each as a new
    // tab in targetFolder — same "still needs Save" rule as any other new
    // file (openFile + writePendingLocal/stageTmpEdit, same pattern
    // commitNewItem and applyAIEdit's new-file branch already use).
    // Directories can't be read this way (the plain File API has no
    // recursive folder-reading without the non-standard
    // webkitGetAsEntry() API) — silently skipped rather than erroring,
    // same as dropping a folder just does nothing.
    // readEntries only returns up to 100 entries per call — has to be
    // called repeatedly until it comes back empty to see everything in a
    // folder, per the (non-standard but universally implemented) File and
    // Directory Entries API's own documented behavior.
    async function readAllDirectoryEntries(reader: FileSystemDirectoryReader): Promise<FileSystemEntry[]> {
      const entries: FileSystemEntry[] = [];
      for (;;) {
        const batch = await new Promise<FileSystemEntry[]>((resolve, reject) => reader.readEntries(resolve, reject));
        if (!batch.length) return entries;
        entries.push(...batch);
      }
    }

    // Recursively walks one dropped entry (file or folder) into a flat
    // {path, file} list, relative-pathed from wherever the drop started —
    // dropping a folder named "src" containing "a.txt" yields "src/a.txt".
    async function collectFilesFromEntry(entry: FileSystemEntry, basePath: string, out: {path: string, file: File}[]): Promise<void> {
      const path = basePath ? `${basePath}/${entry.name}` : entry.name;
      if (entry.isFile) {
        const file = await new Promise<File>((resolve, reject) => (entry as FileSystemFileEntry).file(resolve, reject));
        out.push({path, file});
      } else if (entry.isDirectory) {
        const children = await readAllDirectoryEntries((entry as FileSystemDirectoryEntry).createReader());
        for (const child of children) await collectFilesFromEntry(child, path, out);
      }
    }

    // Binary files (PDFs, images, archives, ...) aren't something this
    // plain-text editor can hold — file.text() below would silently mangle
    // one (and, for anything but a small file, blow the localStorage quota
    // in writePendingLocal with an unhandled rejection instead of a clear
    // error). Same heuristic git itself uses to decide "is this diffable as
    // text": a NUL byte anywhere in the first few KB reliably marks binary
    // content — real text, in any encoding this editor supports, never
    // contains one this early.
    const BINARY_SNIFF_BYTES = 8000;
    async function isLikelyBinary(file: File): Promise<boolean> {
      const head = await file.slice(0, BINARY_SNIFF_BYTES).arrayBuffer();
      return new Uint8Array(head).includes(0);
    }

    // Reads every dropped file's (or folder's, recursively) content and
    // opens/stages each as a new tab in targetFolder — same "still needs
    // Save" rule as any other new file (openFile + writePendingLocal/
    // stageTmpEdit, same pattern commitNewItem and applyAIEdit's new-file
    // branch already use).
    async function handleExternalFileDrop(dataTransfer: DataTransfer, targetFolder: string): Promise<void> {
      const collected: {path: string, file: File}[] = [];
      // webkitGetAsEntry is what unlocks folder support (DataTransfer.files
      // alone is always a flat file list, even for a dropped folder) — every
      // current browser has it under this name despite the prefix. Checked
      // per item, not once up front: it returns null for anything that
      // isn't a real OS-originated drag (synthetic DataTransfers included),
      // so a single dropped plain file still needs its own getAsFile()
      // fallback even in a browser that generally supports entries.
      // Both calls must happen synchronously across every item before any
      // await — the browser ends the drop event's DataTransfer lifetime as
      // soon as this handler first suspends, so awaiting mid-loop (e.g. per
      // item) silently drops every item after the first on a multi-file drop.
      const entries: FileSystemEntry[] = [];
      const plainFiles: File[] = [];
      for (const item of dataTransfer.items) {
        const entry = typeof item.webkitGetAsEntry === 'function' ? item.webkitGetAsEntry() : null;
        if (entry) {
          entries.push(entry);
          continue;
        }
        const file = item.getAsFile();
        if (file) plainFiles.push(file);
      }
      for (const entry of entries) await collectFilesFromEntry(entry, '', collected);
      for (const file of plainFiles) collected.push({path: file.name, file});

      for (const {path: relPath, file} of collected) {
        const path = targetFolder ? `${targetFolder}/${relPath}` : relPath;
        if (openTabs.has(path)) {
          showErrorToast(i18n.errorFileExists.replace('%s', path));
          continue;
        }
        if (await isLikelyBinary(file)) {
          showErrorToast(i18n.errorBinaryFile.replace('%s', path));
          continue;
        }
        let content: string;
        try {
          content = await file.text();
        } catch {
          showErrorToast(i18n.errorLoadFile.replace('%s', path));
          continue;
        }
        insertFilePath(treeRoot, path, true);
        renderTree();
        writePendingLocal(path, content, Date.now()); // see the same call in commitNewItem for why this can't wait for openFile
        openFileAndStage(path, content);
      }
    }

    function wireDropTarget(el: HTMLElement, targetFolder: string): void {
      el.addEventListener('dragover', (e) => {
        if (!dragSourcePath && !hasExternalFiles(e)) return;
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
        if (source) {
          moveFileToFolder(source, targetFolder);
        } else if (e.dataTransfer?.files.length) {
          handleExternalFileDrop(e.dataTransfer, targetFolder);
        }
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
        // Same reasoning as applyAIEdit's new-tab branch: openFile only
        // wires up staging on the textarea's own 'change' event, which
        // never fires for a brand-new file nobody has typed into yet — an
        // empty file created here and refreshed before its first keystroke
        // was never staged at all, not even as an empty entry, so it just
        // vanished. Stage it the moment it's created instead of waiting on
        // a change that might never come before a refresh.
        writePendingLocal(fullValue, '', Date.now());
        openFileAndStage(fullValue, '');
      } else {
        insertFolderPath(treeRoot, fullValue);
        expandedFolders.add(fullValue);
        renderTree();
      }
    }

    function buildNewItemRow(kind: 'file' | 'folder', parent: string, depth: number): HTMLElement {
      const iconSvg = kind === 'file' ? svg('octicon-file', 14) : svg('octicon-file-directory-fill', 14);
      const placeholder = kind === 'file' ? i18n.newFilePlaceholder : i18n.newFolderPlaceholder;
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

    // Fire-and-forget, debounced like stageTmpEdit — a click toggling a
    // folder shouldn't wait on a network round trip, and clicking several
    // in a row (opening a nested path) only needs the final state saved.
    let expandedFoldersSaveTimer: ReturnType<typeof setTimeout> | undefined;
    function saveExpandedFolders(): void {
      clearTimeout(expandedFoldersSaveTimer);
      expandedFoldersSaveTimer = setTimeout(async () => {
        try {
          await POST(foldersUrl, {data: {paths: [...expandedFolders]}});
        } catch {
          // best-effort — worst case the sidebar just reopens with yesterday's state next visit
        }
      }, TMP_SAVE_DEBOUNCE_MS);
    }

    function renderTree(): void {
      if (pendingNewItem) expandedFolders.add(pendingNewItem.parent); // make sure its folder is open so the input is visible
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
        const collapsed = !expandedFolders.has(folder.path);
        const chevronSvg = svg(collapsed ? 'octicon-chevron-right' : 'octicon-chevron-down', 12);
        const cachedFolderIcon = iconCache.get(folder.path);
        const folderSvg = cachedFolderIcon ?
          (collapsed ? cachedFolderIcon.icon : cachedFolderIcon.iconOpen) :
          svg(collapsed ? 'octicon-file-directory-fill' : 'octicon-file-directory-open-fill', 14);
        const row = createElementFromHTML<HTMLElement>(
          `<div class="company-workspace-tree-row company-workspace-tree-folder" style="padding-left:${depth * 16}px" data-tooltip-content="${collapsed ? i18n.folderClosed : i18n.folderOpen}">` +
          `<span class="company-workspace-tree-chevron">${chevronSvg}</span>${folderSvg}` +
          `<span class="company-workspace-tree-name"></span></div>`,
        );
        row.querySelector('.company-workspace-tree-name')!.textContent = folder.name;
        row.classList.toggle('selected', folder.path === selectedFolderPath);
        row.addEventListener('click', () => {
          if (collapsed) expandedFolders.add(folder.path);
          else expandedFolders.delete(folder.path);
          selectedFolderPath = folder.path;
          renderTree();
          saveExpandedFolders();
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
          `${fileSvg}<span class="company-workspace-tree-name"></span>` +
          `<button type="button" class="company-workspace-tree-delete" data-tooltip-content="${i18n.deleteFile}" aria-label="${i18n.deleteFile}">${svg('octicon-x', 12)}</button></div>`,
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
        row.querySelector('.company-workspace-tree-delete')!.addEventListener('click', (e) => {
          e.stopPropagation(); // don't also trigger the row's own open-file click above
          deleteFile(fullPath);
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
        // Neither branch above fires the textarea's own 'change' event
        // (that's what schedules stageTmpEdit for a person's own typing),
        // so an AI edit never reached tmp staging — a refresh before
        // clicking Save silently lost it. Stage it directly instead of
        // debouncing: this is one discrete write, not a keystroke stream.
        clearTimeout(existing.tmpSaveTimer);
        stageTmpEdit(existing);
        return;
      }
      insertFilePath(treeRoot, path, true);
      renderTree();
      // Same reasoning as above, but the localStorage write can't wait for
      // stageTmpEdit here: openFile is async (it awaits createCodeEditor
      // mounting CodeMirror), so calling stageTmpEdit only once that
      // promise resolves left a real gap — a refresh landing before the
      // editor finished mounting (easy to hit right after the AI creates
      // several files in a row) still lost the content, even after the
      // fix above. Writing to localStorage synchronously, right now,
      // closes that gap; the network POST + localStorage cleanup can still
      // wait for the tab to exist.
      writePendingLocal(path, content, Date.now());
      openFileAndStage(path, content);
    }

    // AI-proposed deletion (delete_file, company/mcp.go) — same staged/
    // still-needs-Save state deleteFile above puts a person's own hover-X
    // click into, minus the window.confirm(): a blocking native dialog
    // popping up mid-stream, for something no more destructive or harder to
    // undo before Save than any other AI-proposed change, would be a
    // jarring, disproportionate gate here specifically. The chat's own
    // status line (company-ai-chat.ts) already says plainly what happened.
    function applyAIDelete(path: string): void {
      const tab = openTabs.get(path);
      if (tab) {
        clearTimeout(tab.tmpSaveTimer);
        localStorage.removeItem(pendingKey(repoLink, branch, path));
        clearTmpEditRemote(path);
        tab.tabEl.remove();
        tab.pane.remove();
        openTabs.delete(path);
        if (tab.serverPath) {
          pendingDeletes.add(tab.serverPath);
          stageTmpDelete(tab.serverPath);
        }
      } else {
        // Not open this session — its tree path is its real server path,
        // same assumption deleteFile's own equivalent branch makes.
        pendingDeletes.add(path);
        stageTmpDelete(path);
      }
      removeFilePath(treeRoot, path);

      if (activePath === path) {
        const next = openTabs.keys().next().value;
        if (next) activateTab(next);
        else {
          activePath = null;
          chatHandle?.setActiveFilePath(null);
        }
      }
      renderTree();
    }

    // AI-proposed rename (rename_file, company/mcp.go) — applyPathRename is
    // the same core a person's own drag-and-drop move uses; brings the
    // renamed file into view afterward the same way applyAIEdit brings an
    // edited one into view, so what the AI just did is immediately visible.
    async function applyAIRename(fromPath: string, toPath: string): Promise<void> {
      if (!(await applyPathRename(fromPath, toPath))) return;
      renderTree();
      activateTab(toPath);
    }

    const aiChatEl = el.querySelector<HTMLElement>('.company-workspace-ai-chat');
    if (aiChatEl) {
      chatHandle = initChatPanel(aiChatEl, {
        sendUrl: aiUrl,
        onEdit: applyAIEdit,
        onDelete: applyAIDelete,
        onRename: applyAIRename,
        emptyStateText: aiChatEl.getAttribute('data-ai-empty-state')!,
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

    // Someone else saved a newer version of this exact file while it was
    // still open here (company/workspace.go's WorkspaceSave returned 409,
    // Gitea's own optimistic-lock check on the blob SHA this tab started
    // from). Rather than silently overwrite their change or silently
    // discard this person's own edit, run a real 3-way merge against the
    // common ancestor the 409 carries (company-conflict.ts): edits that
    // touched different lines just combine, and only lines both sides
    // changed differently become <<<<<<< / ======= / >>>>>>> blocks with
    // one-click resolution buttons. tab.baseSha is advanced to the version
    // just merged in as "theirs" so that retry compares against the right
    // baseline, not the stale one that just caused this conflict.
    function handleConflict(conflict: WorkspaceConflict): void {
      const tab = openTabs.get(conflict.path);
      if (!tab) return; // the path came from our own save request — should always still be open
      activateTab(conflict.path);

      const theirsLabel = i18n.conflictTheirs.replace('%s', conflict.serverAuthor).replace('%s', formatDatetime(conflict.serverDate * 1000));
      const {text, conflicts} = buildConflictText(conflict.baseContent ?? null, tab.textarea.value, conflict.serverContent, i18n.conflictYours, theirsLabel);

      if (tab.editor) {
        const {view} = tab.editor;
        view.dispatch({changes: {from: 0, to: view.state.doc.length, insert: text}});
      } else {
        tab.textarea.value = text;
      }
      tab.baseSha = conflict.serverSha;
      if (conflicts === 0) {
        // every edit landed on different lines — merged cleanly, nothing
        // to resolve, just review and save again
        setStatus(i18n.statusAutoMerged.replace('%s', conflict.path));
        return;
      }
      const message = i18n.statusConflict.replace('%s', conflict.path);
      setStatus(message);
      showErrorToast(message);
    }

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
        // Sent so the server can tell whether someone else saved a newer
        // version of this exact file since it was opened here — undefined
        // for a file that never existed before (nothing to compare
        // against). Not sent for deletes below: those aren't tracked back
        // to a specific tab/baseline once queued in pendingDeletes, so a
        // delete can't currently detect "someone else changed this file
        // first" — a narrower gap than the edit-vs-edit case this exists
        // for, left alone for now.
        baseSha: tab.baseSha,
      }));
      const deletes = [...pendingDeletes].map((path) => ({path, deleted: true}));
      if (!files.length && !deletes.length) {
        setStatus(i18n.statusNoChanges);
        return;
      }
      // Unresolved conflict markers about to be committed as literal file
      // content — almost always a mistake for this editor's audience, so
      // ask first (git itself allows it, so proceeding stays possible).
      const unresolved = changedTabs.find((tab) => conflictMarkerPattern.test(tab.textarea.value));
      if (unresolved && !window.confirm(i18n.conflictUnresolved.replace('%s', unresolved.path))) {
        return;
      }
      saveButton.disabled = true;
      setStatus(i18n.statusSaving);
      try {
        const resp = await POST(saveUrl, {data: {files: [...files, ...deletes]}});
        if (resp.status === 409) {
          const {conflict} = await resp.json() as {conflict: WorkspaceConflict};
          handleConflict(conflict);
          return;
        }
        if (!resp.ok) throw new Error(String(resp.status));
        const {shas} = await resp.json() as {shas: Record<string, string>};
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
          tab.baseSha = shas[tab.path] ?? tab.baseSha; // see the comment on WorkspaceSave's own "shas" response field
        }
        pendingDeletes.clear();
        setStatus(i18n.statusSaved);
      } catch {
        setStatus('');
        showErrorToast(i18n.errorSaveFailed);
      } finally {
        saveButton.disabled = false;
      }
    });

    // Fetched before the tree paints at all (below) so the very first
    // render already reflects last visit's expand/collapse state, instead
    // of a flash of "everything collapsed" that then reopens a moment
    // later once this lands.
    try {
      const resp = await GET(foldersUrl);
      if (resp.ok) {
        const {paths} = await resp.json() as {paths: string[]};
        for (const path of paths) expandedFolders.add(path);
      }
    } catch {
      // best-effort — worst case the sidebar just starts fully collapsed this once
    }

    try {
      const resp = await GET(`${repoLink}/tree-list/branch/${encodeURIComponent(branch)}`);
      // A repo with zero commits yet has no branch ref for this to list —
      // native tree-list (repo/treelist.go) 500s against that rather than
      // returning an empty array. RedirectAwayFromEmptyRepo
      // (company/workspace.go) keeps most people from ever reaching this
      // page in that state, but an exempted admin still can — treat the
      // failure as "nothing to show yet" instead of a scary error toast,
      // since starting from an empty tree here is completely valid (same
      // as any other empty folder) and creating the first file still works.
      if (resp.ok) {
        const paths: string[] = await resp.json();
        for (const path of paths) insertFilePath(treeRoot, path, false); // server order, appended below any new entries
        renderTree(); // paint the structure immediately with generic icons

        const folderPaths: string[] = [];
        collectFolderPaths(treeRoot, folderPaths);
        await Promise.all(folderPaths.map(fetchFolderIcons));
        renderTree(); // repaint once the real per-file-type icons are in
      }
    } catch {
      // same reasoning as the !resp.ok branch above
    }

    await recoverPendingEdits();
  });
}
