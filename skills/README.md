# az agent skills

Skills for coding agents (Claude Code and compatible harnesses) working in
**other** projects that use `az` as a dependency. They encode how to integrate
`github.com/go-again/az` correctly so an agent doesn't have to re-derive the API,
level trade-offs, or pooling gotchas.

| Skill | Use when |
|-------|----------|
| [`az-integration`](az-integration/SKILL.md) | Adding/wiring az: one-shot `Compress`/`Decompress`, streaming `Writer`/`Reader`, level choice, errors, wire-format compatibility. |
| [`az-pooling`](az-pooling/SKILL.md) | High-throughput per-chunk/per-request compression with pooled `Encoder`/`Decoder` (`EncodeAll`/`DecodeAll`), and pooled streaming `Writer`s. |
| [`az-http`](az-http/SKILL.md) | HTTP traffic: `azhttp` response-compression middleware, compressed request bodies, and the client `Transport`. |

## Install into a consuming project

Copy the skill directories into that project's skills folder:

```sh
# from the consuming project's root
mkdir -p .claude/skills
cp -R "$(go list -m -f '{{.Dir}}' github.com/go-again/az)"/skills/az-* .claude/skills/
```

Or vendor them manually — each skill is a self-contained `SKILL.md` with YAML
frontmatter (`name`, `description`) that the agent matches on relevance.

> These are for **consumers**. If you're developing `az` itself, see
> [`../AGENTS.md`](../AGENTS.md).
