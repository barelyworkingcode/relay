// Home/Work mode as pure functions over project and default-project state.
// Mirrors the Go side's "missing or unknown reads as both" rule: nothing here
// compares a raw mode string except normalizeMode, the one place that does.
// No state, window or ipc: app.js owns the project list and the
// default_project block and passes them in.
//
// A project is { id, name, kind, mode?, ... } as projectToView emits it:
// kind is "" or "local" for a local project, "remote" for an access profile;
// mode is absent for "both". defaults is the raw default_project block,
// { home?: "<id>", work?: "<id>" }, or null when never configured.

import { esc } from './pure.js';

const MODE_HOME = 'home';
const MODE_WORK = 'work';
const MODE_BOTH = 'both';

const DEFAULT_MODES = [MODE_HOME, MODE_WORK];

const MODE_LABELS = { home: 'Home', work: 'Work', both: 'Both' };

function normalizeMode(mode) {
    return (mode === MODE_HOME || mode === MODE_WORK) ? mode : MODE_BOTH;
}

function projMode(p) {
    return normalizeMode(p && p.mode);
}

function modeIncludes(mode, target) {
    const m = normalizeMode(mode);
    return m === MODE_BOTH || m === target;
}

function modeLabel(mode) {
    return MODE_LABELS[mode] || MODE_LABELS[MODE_BOTH];
}

function isDefaultEligible(project, mode) {
    if (!project || project.kind === 'remote') return false;
    return modeIncludes(projMode(project), mode);
}

function eligibleDefaultProjects(projects, mode) {
    return (projects || []).filter(p => isDefaultEligible(p, mode));
}

function effectiveDefaultId(projects, defaults, mode) {
    const id = defaults && defaults[mode];
    if (!id) return '';
    const p = (projects || []).find(pr => pr.id === id);
    return isDefaultEligible(p, mode) ? id : '';
}

function defaultModesFor(defaults, projectId) {
    if (!defaults || !projectId) return [];
    return DEFAULT_MODES.filter(mode => defaults[mode] === projectId);
}

// defaultProjectGaps(projects, null) is [] on purpose: no default_project
// block means the operator has never used modes, so there is nothing to flag
// yet -- see the contract's "upgraded installs don't gain two rows".
function defaultProjectGaps(projects, defaults) {
    if (!defaults) return [];
    const gaps = [];
    for (const mode of DEFAULT_MODES) {
        const id = defaults[mode];
        if (!id) {
            gaps.push({ mode: mode, reason: 'unset', projectId: '', projectName: '' });
            continue;
        }
        const p = (projects || []).find(pr => pr.id === id);
        if (!p) {
            gaps.push({ mode: mode, reason: 'missing', projectId: id, projectName: '' });
            continue;
        }
        if (!isDefaultEligible(p, mode)) {
            gaps.push({ mode: mode, reason: 'ineligible', projectId: id, projectName: p.name || '' });
        }
    }
    return gaps;
}

function gapText(gap) {
    const label = modeLabel(gap.mode);
    switch (gap.reason) {
        case 'unset': return 'No default project for ' + label + '.';
        case 'missing': return 'The default project for ' + label + ' no longer exists.';
        case 'ineligible': return esc(gap.projectName) + ' can\'t be the default project for ' + label + '.';
        default: return '';
    }
}

function defaultProjectAttentionRows(projects, defaults) {
    return defaultProjectGaps(projects, defaults).map(gap => ({ text: gapText(gap), tab: 'projects' }));
}

export {
    DEFAULT_MODES, projMode, modeIncludes, modeLabel, isDefaultEligible,
    eligibleDefaultProjects, effectiveDefaultId, defaultModesFor,
    defaultProjectGaps, defaultProjectAttentionRows
};
