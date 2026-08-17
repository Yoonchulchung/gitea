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

  function showEmptyState(text: string): void {
    emptyStateEl = createElementFromHTML<HTMLElement>('<div class="company-ai-chat-empty"></div>');
    emptyStateEl.innerHTML = `<div>${text}</div>`;
    messagesEl.append(emptyStateEl);
  }

  if (el.getAttribute('data-ai-ready') !== 'true') {
    showEmptyState(attr(el, 'data-ai-not-configured'));
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

  function setSending(sending: boolean): void {
    sendButton.disabled = sending && !abortController; // disabled only while genuinely unable to act; "sending" itself repurposes the button as Stop
    sendButton.classList.toggle('sending', sending);
    sendButton.innerHTML = sending ? svg('octicon-stop', 14) : svg('octicon-arrow-up', 16);
    sendButton.setAttribute('aria-label', sending ? stopLabel : sendLabel);
  }

  async function send(): Promise<void> {
    const instruction = inputEl.value.trim();
    if (!instruction || abortController) return;

    addMessage('user', instruction);
    history.push({role: 'user', content: instruction});
    inputEl.value = '';
    inputEl.style.height = 'auto';

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
    }
  }

  sendButton.addEventListener('click', () => {
    if (abortController) {
      abortController.abort();
      return;
    }
    send();
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
      send();
    }
  });
  inputEl.addEventListener('input', () => {
    inputEl.style.height = 'auto';
    inputEl.style.height = `${Math.min(inputEl.scrollHeight, 220)}px`; // keep in sync with .company-ai-chat-input's max-height in workspace.tmpl
  });

  return {setActiveFilePath};
}
