# Events

## 6. Other

| Event | When |
|---|---|
| `not.in.seven` | x |

## 7. The catalogue

### Server

| Event | When written | Doors | Fields |
|---|---|---|---|
| `server.stopped` | On exit | `relay serve` | `pid` |

### Projects

| Event | When written | Doors | Fields |
|---|---|---|---|
| `project.list` | Before the list | `GET /api/projects` | `count` |
| `project.create` | In the core | `POST /api/projects` | `project_id` |

## 8. After

| Event | When |
|---|---|
| `not.in.seven.either` | x |
