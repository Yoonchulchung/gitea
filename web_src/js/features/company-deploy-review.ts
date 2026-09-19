import {registerGlobalInitFunc} from '../modules/observer.ts';
import {fillModelOptions, initChatPanel} from './company-ai-chat.ts';

// The assistant on a Deploy Request PR's sidebar
// (custom/templates/company/deploy_review_sidebar.tmpl): the same chat
// panel as the workspace editor's, read-only — the server offers no
// write tools, so no edit events arrive. See company/deploy_review.go.
export function initCompanyDeployReview() {
  registerGlobalInitFunc('initCompanyDeployReview', (el: HTMLElement) => {
    const modelSelect = el.querySelector<HTMLSelectElement>('.company-ai-chat-model');
    initChatPanel(el, {
      sendUrl: el.getAttribute('data-send-url')!,
      emptyStateText: el.getAttribute('data-ai-empty-state')!,
      getContext: () => ({activePath: null, openFiles: [], model: modelSelect?.value ?? ''}),
    });
    if (modelSelect) fillModelOptions(modelSelect);
  });
}
