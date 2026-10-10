# Selection fixture

## Areas

```yaml
areas:
  alpha:
    code: [alpha/**, cmd/relay/**]
  beta:
    code: [beta/**, cmd/relaysessions/**]
  gamma:
    code: [gamma/**]
  every-test:
    code: [e2e/harness/**, e2e/go.mod, go.mod]
  no-test:
    code: [scripts/**, e2e/coverage/proof/**]
```

## Goals

### G1 · Alpha
Areas: alpha.

| ID | Feature | Claim | Simple door | Power door | Doors | Gate | Proof | Test |
|---|---|---|---|---|---|---|---|---|
| `G1.01` | One | x | n/a | n/a | none | none | `event:alpha.one=ok` | `e2e:TestAlphaOne` |
| `G1.02` | Two | x | n/a | n/a | none | none | `event:alpha.two=ok` | `e2e:TestAlphaTwo` |

### G2 · Beta
Areas: beta.

| ID | Feature | Claim | Simple door | Power door | Doors | Gate | Proof | Test |
|---|---|---|---|---|---|---|---|---|
| `G2.01` | One | x | n/a | n/a | none | none | `event:beta.one=ok` | `e2e:TestBetaOne` |
| `G2.02` | Helper | x | n/a | n/a | none | none | `event:beta.helper=ok` | `e2e:TestHelperOnly` |

### G3 · Gamma
Areas: gamma.

| ID | Feature | Claim | Simple door | Power door | Doors | Gate | Proof | Test |
|---|---|---|---|---|---|---|---|---|
| `G3.01` | One | x | n/a | n/a | none | none | `event:gamma.one=ok` | `e2e:TestGammaOne` |
