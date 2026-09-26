import type {Issue} from '../types.ts';

// the getIssueIcon/getIssueColorClass logic should be kept the same as "templates/shared/issueicon.tmpl"

export function getIssueIcon(issue: Issue) {
  if (issue.pull_request) {
    if (issue.state === 'open') {
      if (issue.pull_request.draft) {
        return 'octicon-git-pull-request-draft'; // WIP PR
      }
      return 'octicon-git-pull-request'; // Open PR
    } else if (issue.pull_request.merged) {
      // company: merging a Deploy Request is how it is approved, so the icon
      // says approved — see custom/templates/shared/issueicon.tmpl.
      return 'octicon-check-circle-fill'; // Approved (merged) PR
    }
    return 'octicon-git-pull-request-closed'; // Closed PR
  }

  if (issue.state === 'open') {
    return 'octicon-issue-opened'; // Open Issue
  }
  return 'octicon-issue-closed'; // Closed Issue
}

export function getIssueColorClass(issue: Issue) {
  if (issue.pull_request) {
    if (issue.state === 'open') {
      if (issue.pull_request.draft) {
        return 'tw-text-text-light'; // WIP PR
      }
      return 'tw-text-green'; // Open PR
    } else if (issue.pull_request.merged) {
      return 'tw-text-green'; // Approved (merged) PR
    }
    return 'tw-text-red'; // Closed PR
  }

  if (issue.state === 'open') {
    return 'tw-text-green'; // Open Issue
  }
  return 'tw-text-red'; // Closed Issue
}
