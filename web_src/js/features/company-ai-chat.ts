import {POST} from '../modules/fetch.ts';
import {createElementFromHTML} from '../utils/dom.ts';
import {svg} from '../svg.ts';

// Shared chat-sidebar module for both AI surfaces (docs/company/ai-agent.md):
// the workspace editor's "Ask AI" panel (web_src/js/features/company-workspace.ts)
// and, later, the PR-review sidebar. Talks to a streaming ndjson endpoint —
// company/workspace_ai.go's WorkspaceAI is the only one that exists so far —
// one JSON object per line:
//   {"type":"text","delta":"..."}                — append to the current reply
//   {"type":"tool","name":"read_file","args":{}}  — status line, not part of the reply text
//   {"type":"edit","path":"...","content":"..."}  — a proposed file edit
//   {"type":"done"}
//   {"type":"error","message":"..."}

type ChatTurn = {
  role: 'user' | 'assistant';
  content: string;
};

export type OpenFile = {
  path: string;
  content: string;
};

export type ChatContext = {
  activePath: string | null;
  // Every open tab's live content, not just the active one — read_file
  // (company/workspace_ai.go) checks this before falling back to git, so
  // an unsaved edit in a tab that isn't even on screen right now is still
  // visible to the model. Sending only the active tab was a real bug:
  // switch tabs before saving and the AI would silently read stale,
  // already-committed content for whatever you'd just been editing.
  openFiles: OpenFile[];
};

export type ChatPanelOptions = {
  sendUrl: string;
  // Shown once, before the first message — what this particular sidebar is
  // for ("파일을 읽고 수정을 제안합니다" vs a read-only PR reviewer, say).
  emptyStateText?: string;
  // Called once per {"type":"edit"} event — the workspace editor drops
  // these into open tabs; a read-only surface (PR sidebar) can omit this.
  onEdit?: (path: string, content: string) => void;
  // Called once per {"type":"delete"} / {"type":"rename"} event — same
  // "proposal only, still needs Save" rule as onEdit above. Omit on a
  // read-only surface the same way.
  onDelete?: (path: string) => void;
  // May need to openExistingFile a not-yet-open tab first (applyPathRename,
  // company-workspace.ts) — allowed to be async; called fire-and-forget
  // either way, nothing here awaits it.
  onRename?: (fromPath: string, toPath: string) => void | Promise<void>;
  // Called fresh right before every send — whatever it returns gets
  // attached to that one request as ambient context, Claude-Code-style: an
  // unqualified "이거 고쳐줘" then means the file already on screen, without
  // the model having to read_file it first. Not called at any other time,
  // so there's no separate state to keep in sync — see setActiveFilePath
  // below for the (separate) filename chip, which does need to track tab
  // switches live.
  getContext?: () => ChatContext;
};

export type ChatPanelHandle = {
  // Updates the filename chip on the input's toolbar row — call this
  // whenever the active tab changes (company-workspace.ts's activateTab).
  // Display-only: the actual content sent with a message always comes
  // fresh from getContext above, not from whatever was passed here.
  setActiveFilePath: (path: string | null) => void;
};

// initChatPanel wires up a chat sidebar inside el, which must already
// contain a `.company-ai-chat-messages`, `.company-ai-chat-inputwrap` with
// a `.company-ai-chat-input` (textarea), `.company-ai-chat-context` (the
// filename chip), and `.company-ai-chat-send` (button) inside it — see
// custom/templates/company/workspace.tmpl for the markup this expects. The
// panel itself always renders (docs/company/ai-agent.md — people should be
// able to see the feature exists even before AI is set up); el's own
// `data-ai-ready` says whether AI is actually usable yet, checked once
// here rather than only discovered after a failed send; `data-ai-*`
// otherwise carries every locale-translated string this module needs
// (see custom/templates/company/workspace.tmpl) — none of the text here
// is hardcoded, so it follows whatever language the page itself renders
// in, not just Korean.
// Falls back to '' instead of null — a missing data-ai-* attribute (a
// stale page from before one was added, say) previously meant a status
// message or confirm dialog showing the literal word "null".
function attr(el: Element, name: string): string {
  return el.getAttribute(name) ?? '';
}

export function initChatPanel(el: HTMLElement, opts: ChatPanelOptions): ChatPanelHandle {
  const messagesEl = el.querySelector<HTMLElement>('.company-ai-chat-messages')!;
  const inputEl = el.querySelector<HTMLTextAreaElement>('.company-ai-chat-input')!;
  const sendButton = el.querySelector<HTMLButtonElement>('.company-ai-chat-send')!;
  const contextEl = el.querySelector<HTMLElement>('.company-ai-chat-context')!;
  sendButton.innerHTML = svg('octicon-arrow-up', 16);

  function setActiveFilePath(path: string | null): void {
    contextEl.replaceChildren();
    if (!path) {
      contextEl.classList.add('tw-hidden');
      return;
    }
    contextEl.classList.remove('tw-hidden');
    contextEl.insertAdjacentHTML('afterbegin', svg('octicon-file', 12));
    contextEl.append(path);
  }

  const history: ChatTurn[] = [];
  let abortController: AbortController | null = null;
  let emptyStateEl: HTMLElement | null = null;
  // Messages sent while a previous one is still streaming — send() itself
  // can't just start a second, overlapping request (abortController is one
  // slot, and interleaving two streams into the same message list would
  // scramble which reply belongs to which turn). Queued instead of
  // rejected/dropped: each still lands in the chat immediately, in order,
  // and runs for real the moment the current turn's stream actually ends.
  const queuedInstructions: string[] = [];

  function showEmptyState(text: string): void {
    emptyStateEl = createElementFromHTML<HTMLElement>('<div class="company-ai-chat-empty"></div>');
    emptyStateEl.innerHTML = `<div>${text}</div>`;
    messagesEl.append(emptyStateEl);
  }

  // Separate from showEmptyState above: that one trusts its caller's HTML
  // (opts.emptyStateText is server-controlled markup, not user input), but
  // an <a href> can't travel through a data-* attribute as markup — Gitea's
  // template auto-escaping strips tags rather than round-tripping them
  // through an attribute value, so ctx.Locale.Tr's own <a> tag never
  // survives into the DOM that way. Building the link as a real element
  // from separate plain-text/URL attributes sidesteps that entirely.
  function showNotConfiguredState(message: string, linkLabel: string, linkURL: string): void {
    emptyStateEl = createElementFromHTML<HTMLElement>('<div class="company-ai-chat-empty"></div>');
    const messageEl = document.createElement('div');
    messageEl.textContent = message;
    emptyStateEl.append(messageEl);
    if (linkURL) {
      const link = document.createElement('a');
      link.href = linkURL;
      link.textContent = linkLabel;
      emptyStateEl.append(link);
    }
    messagesEl.append(emptyStateEl);
  }

  if (el.getAttribute('data-ai-ready') !== 'true') {
    showNotConfiguredState(attr(el, 'data-ai-not-configured'), attr(el, 'data-ai-not-configured-link'), attr(el, 'data-ai-not-configured-url'));
    inputEl.disabled = true;
    inputEl.placeholder = attr(el, 'data-ai-not-configured-placeholder');
    sendButton.disabled = true;
    return {setActiveFilePath};
  }

  if (opts.emptyStateText) showEmptyState(opts.emptyStateText);

  // addMessage returns the element new text should be appended to —
  // everything appends directly to the bubble it returns.
  function addMessage(role: 'user' | 'assistant' | 'status' | 'error', text: string): HTMLElement {
    emptyStateEl?.remove();
    emptyStateEl = null;

    const bubble = createElementFromHTML<HTMLElement>(`<div class="company-ai-chat-message company-ai-chat-message-${role}"></div>`);
    const textTarget = bubble;
    textTarget.textContent = text;
    messagesEl.append(bubble);
    messagesEl.scrollTop = messagesEl.scrollHeight;
    return textTarget;
  }

  const stopLabel = attr(el, 'data-ai-stop');
  const sendLabel = attr(sendButton, 'aria-label'); // set server-side from company.workspace.ai_send
  const requestFailedText = attr(el, 'data-ai-request-failed');
  const readStatusTemplate = attr(el, 'data-ai-read-status'); // has a literal "%s" placeholder — see the comment on handleToolEvent below
  const editStatusTemplate = attr(el, 'data-ai-edit-status');
  const deleteStatusTemplate = attr(el, 'data-ai-delete-status');
  const renameStatusTemplate = attr(el, 'data-ai-rename-status'); // has two "%s" placeholders — from, then to

  function setSending(sending: boolean): void {
    sendButton.disabled = sending && !abortController; // disabled only while genuinely unable to act; "sending" itself repurposes the button as Stop
    sendButton.classList.toggle('sending', sending);
    sendButton.innerHTML = sending ? svg('octicon-stop', 14) : svg('octicon-arrow-up', 16);
    sendButton.setAttribute('aria-label', sending ? stopLabel : sendLabel);
  }

  const queueEl = el.querySelector<HTMLElement>('.company-ai-chat-queue');
  // has a literal "%s" placeholder — not "%d": Gitea's server-side Tr()
  // formats with Go's fmt verbs, and %d rejects a string argument (even
  // one only ever meant to survive untouched to be replaced here) with a
  // visible "%!d(string=...)" error text — %s has no such restriction.
  const queuedTemplate = attr(el, 'data-ai-queued');

  function updateQueueIndicator(): void {
    if (!queueEl) return;
    if (queuedInstructions.length === 0) {
      queueEl.textContent = '';
      queueEl.classList.add('tw-hidden');
      return;
    }
    queueEl.textContent = queuedTemplate.replace('%s', String(queuedInstructions.length));
    queueEl.classList.remove('tw-hidden');
  }

  // submit() is what the send button/Enter key actually calls — always
  // records the message right away (addMessage/history), so the
  // conversation reads in the order things were typed regardless of
  // streaming state. runTurn (below) is the part that actually round-trips
  // to the server for one instruction; submit either calls it directly or,
  // if one's already in flight, queues this instruction to run once that
  // one's stream ends (see runTurn's own finally block).
  function submit(): void {
    const instruction = inputEl.value.trim();
    if (!instruction) return;
    inputEl.value = '';
    inputEl.style.height = 'auto';

    addMessage('user', instruction);
    history.push({role: 'user', content: instruction});

    if (abortController) {
      queuedInstructions.push(instruction);
      updateQueueIndicator();
      return;
    }
    runTurn(instruction);
  }

  async function runTurn(instruction: string): Promise<void> {
    // The model can read/write files in between chunks of its own reply —
    // one long bubble built by just appending every delta would visually
    // collapse those tool calls to the bottom, after all the text, no
    // matter when they actually happened mid-stream (this was a real bug:
    // https://github.com/.../issues, reported as "status pills show up
    // after the whole message instead of where the read happened"). Each
    // text segment between tool events gets its own bubble instead, so the
    // status lines land inline, in the order they actually occurred, and
    // text resumes in a fresh bubble below them.
    let replyTarget: HTMLElement | null = null;
    let segmentText = '';
    let replyText = ''; // full reply across every segment, for history

    abortController = new AbortController();
    setSending(true);
    try {
      const resp = await POST(opts.sendUrl, {
        data: {instruction, history: history.slice(0, -1), ...opts.getContext?.() ?? {activePath: null, openFiles: []}},
        signal: abortController.signal,
      });
      if (!resp.ok || !resp.body) throw new Error(String(resp.status));

      const reader = resp.body.getReader();
      const decoder = new TextDecoder();
      let buffer = '';
      const onEvent = (raw: string) => {
        const event = JSON.parse(raw);
        if (event.type === 'text') {
          if (!replyTarget) {
            replyTarget = addMessage('assistant', '');
            segmentText = '';
          }
          segmentText += event.delta;
          replyText += event.delta;
          replyTarget.textContent = segmentText;
          messagesEl.scrollTop = messagesEl.scrollHeight;
          return;
        }
        if (event.type === 'error') {
          addMessage('error', event.message || requestFailedText);
          return;
        }
        if (event.type === 'done') return;
        handleToolEvent(event);
        // Whatever text comes next belongs after this tool call, not
        // appended into the bubble that came before it.
        replyTarget = null;
      };
      for (;;) {
        const {done, value} = await reader.read();
        if (done) break;
        buffer += decoder.decode(value, {stream: true});
        const lines = buffer.split('\n');
        buffer = lines.pop() ?? ''; // last element may be a partial line — keep it for the next chunk
        for (const line of lines) {
          if (line.trim()) onEvent(line);
        }
      }
      if (buffer.trim()) onEvent(buffer);

      if (replyText) history.push({role: 'assistant', content: replyText});
    } catch (err) {
      if ((err as Error).name !== 'AbortError') addMessage('error', requestFailedText);
    } finally {
      abortController = null;
      setSending(false);
      inputEl.focus();
      // Run the next queued instruction, if any — its own turn was already
      // recorded (addMessage/history, in submit() above) the moment it was
      // typed, so this just does the actual round trip now that there's a
      // free slot. Not awaited: this is already inside runTurn's own
      // finally block, and the caller that's waiting on *this* call has
      // nothing further to do once its own turn is done regardless of
      // whether another one keeps going after it.
      const next = queuedInstructions.shift();
      updateQueueIndicator();
      if (next !== undefined) runTurn(next);
    }
  }

  function handleToolEvent(event: Record<string, any>): void {
    switch (event.type) {
      case 'tool':
        // list_files is pure internal exploration (no file-specific info to
        // show) and write_file's own {"type":"edit"} event below already
        // reports the same call more usefully — showing both was reporting
        // the same action twice. read_file is the only "tool" call left
        // worth a line of its own.
        if (event.name === 'read_file' && event.args?.path) {
          addMessage('status', readStatusTemplate.replace('%s', event.args.path));
        }
        break;
      case 'edit':
        addMessage('status', editStatusTemplate.replace('%s', event.path));
        opts.onEdit?.(event.path, event.content);
        break;
      case 'delete':
        addMessage('status', deleteStatusTemplate.replace('%s', event.path));
        opts.onDelete?.(event.path);
        break;
      case 'rename':
        addMessage('status', renameStatusTemplate.replace('%s', event.from).replace('%s', event.to));
        opts.onRename?.(event.from, event.to);
        break;
    }
  }

  sendButton.addEventListener('click', () => {
    // The button reads "Stop" while streaming, but that's only what an
    // empty-input click means — with something typed, clicking it (same as
    // pressing Enter) queues that message instead, exactly like Enter
    // already does below. Only an empty-input click during streaming is
    // unambiguously "stop."
    if (abortController && !inputEl.value.trim()) {
      // An explicit Stop means stop — anything queued behind this turn was
      // only ever going to run once this one finished, so it shouldn't
      // fire right after a stop the person just asked for. It's already
      // in the conversation/history (submit() recorded that immediately),
      // just never actually sent to the server.
      queuedInstructions.length = 0;
      updateQueueIndicator();
      abortController.abort();
      return;
    }
    submit();
  });
  inputEl.addEventListener('keydown', (e) => {
    // e.isComposing is true while an IME (Korean/Japanese/Chinese) is still
    // composing the current syllable — the Enter that confirms it also
    // bubbles here as a normal keydown. Sending on that keystroke clears
    // the field before the IME actually commits the syllable, so it lands
    // in the now-empty box right after — the trailing character "leftover"
    // bug. Skipping composing keydowns entirely leaves that Enter to the
    // IME; the next, real Enter keydown sends normally.
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      submit();
    }
  });
  inputEl.addEventListener('input', () => {
    inputEl.style.height = 'auto';
    inputEl.style.height = `${Math.min(inputEl.scrollHeight, 220)}px`; // keep in sync with .company-ai-chat-input's max-height in workspace.tmpl
  });

  return {setActiveFilePath};
}
