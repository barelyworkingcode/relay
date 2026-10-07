// Chief of Staff settings as pure functions over the project list. Mirrors
// config.ChiefOfStaffUnsuitable in internal/config/chief_of_staff.go: same
// checks, same order, same reason text character for character. No state,
// window or ipc: app.js owns the list and the stored block.
//
// A project is { id, name, kind, host_id?, permission_policy?, allowed_models,
// allowed_templates } as relay's settings carry it.

const CHIEF_OF_STAFF_MODELS = ['haiku', 'sonnet', 'opus'];

function isWildcardList(list) {
    return list.length === 1 && list[0] === '*';
}

function modelAllowed(p, model) {
    const models = p.allowed_models || [];
    return models.length === 0 || isWildcardList(models) || models.includes(model);
}

function templateAllowed(p, id) {
    const templates = p.allowed_templates || [];
    return isWildcardList(templates) || templates.includes(id);
}

// Returns why p cannot run the Chief of Staff on model, or '' when it can.
function chiefOfStaffUnsuitable(p, model) {
    if (p.kind === 'remote') return "It's an access profile.";
    if (p.host_id) return 'It runs on an SSH host.';
    if (p.permission_policy) return 'It has a permission policy.';
    if (!modelAllowed(p, model)) return "It doesn't allow the " + model + ' model.';
    if (!templateAllowed(p, 'claude-code')) return "It doesn't allow the claude-code template.";
    return '';
}

function byName(a, b) {
    return a.name < b.name ? -1 : a.name > b.name ? 1 : 0;
}

function localProjects(projects) {
    return (projects || []).filter(p => p.kind !== 'remote');
}

function suitableChiefOfStaffProjects(projects, model) {
    return localProjects(projects)
        .filter(p => chiefOfStaffUnsuitable(p, model) === '')
        .sort(byName);
}

// One row per local project the model rules out; text is the exact line shown.
// Access profiles are left out.
function unsuitableChiefOfStaffRows(projects, model) {
    return localProjects(projects)
        .sort(byName)
        .map(p => ({ id: p.id, name: p.name, reason: chiefOfStaffUnsuitable(p, model) }))
        .filter(r => r.reason !== '')
        .map(r => ({ id: r.id, name: r.name, reason: r.reason, text: r.name + ': ' + r.reason }));
}

export {
    CHIEF_OF_STAFF_MODELS, chiefOfStaffUnsuitable, suitableChiefOfStaffProjects, unsuitableChiefOfStaffRows
};
