# Feature map fixture

A fixture written to the feature-map grammar. The example below sits in a
fence, so the check does not read it.

```
| ID | Feature | Claim | Simple door | Power door | Doors | Gate | Proof | Test |
|---|---|---|---|---|---|---|---|---|
| G9.99 | Not read | x | n/a | n/a | none | none | `event:never.read` | `e2e:TestNeverRead` |
```

## G1 · Projects

| ID | Feature | Claim | Simple door | Power door | Doors | Gate | Proof | Test |
|---|---|---|---|---|---|---|---|---|
| `G1.01` | List projects | A read credential lists every project. | Settings > Projects | `relay status` | `http:GET /api/projects` | none | `event:project.list=ok#count` `out:http:GET /api/projects#.projects[]` | `e2e:TestProjectList` |
| `G1.02` | Create a project | An approved create adds the project; a denied one adds none. | Settings > New project | `relay project create` | `http:POST /api/projects` `ipc:create_project` `bridge:admin_op:project.create` `cli:relay project create` | `project.grant` | `event:project.create=ok` `event:project.create=denied/presence_refused` `code:http:POST /api/projects#403` | `e2e:TestProjectCreate` `deny e2e:TestProjectCreateDenied@http:POST /api/projects` `deny e2e:TestProjectCreateDeniedCLI@cli:relay project create` |
| `G1.03` | Status | Status prints the overview. | Settings > Overview | `relay status` | `cli:relay status` | none | `out:cli:relay status#.seal_status` | `pending:#289` |
| `G1.04` | Quit Relay | The tray quits the app. | Tray > Quit | exception: tray menu | none | none | `event:server.stopped=ok` | `screen-only` |

## G14 · Instance

| ID | Feature | Claim | Simple door | Power door | Doors | Gate | Proof | Test |
|---|---|---|---|---|---|---|---|---|
| `G14.01` | Release build carries no test seam | A release build answers unknown command. | none | `relay debug clock` | `cli:relay debug clock` | none | `code:cli:relay debug clock#1` | `ci:scripts/check-test-build.sh` |

**Rows proven by CI:** `G14.01`.

## Threat-model promises

| Promise | Attacker | Asset | Promise (short quote) | Rows |
|---|---|---|---|---|
| `TM4.1` | A peer | Config | No config change without a credential | `G1.02` |
| `TMT.1` | A release user | The binary | The test seams are absent from release | `G14.01` |

## Retired IDs

`G1.90` `G1.91`
