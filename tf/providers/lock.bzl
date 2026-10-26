"""Reading a `.terraform.lock.hcl` for `tf_providers.from_lock`.

A pure function, like those in `source.bzl`, so //tf/providers/tests can
exercise it without a module graph. Errors are returned, not raised: the
extension decides which ones abort evaluation and which become a repository
that fails only when fetched (so `@tf_providers//:pin` still runs and can
rewrite the lock).

The parser understands the subset of HCL that lock files use, as written by
`tofu`, `terraform`, Renovate and Dependabot: `provider "<address>" { ... }`
blocks whose attributes are strings or lists of strings. Unknown attributes
are skipped; anything else is an error.
"""

load(":source.bzl", "DEFAULT_HOST", "is_sha256")

visibility(["//tf/providers/..."])

_PUNCTUATION = "{}[]=,"
_IDENT_CHARS = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-."
_ESCAPES = {"\"": "\"", "\\": "\\", "n": "\n", "t": "\t", "r": "\r"}

def _tokenize(text):
    """Split `text` into `(kind, value, line)` tuples; kind is `s`, `i` or `p`."""
    tokens = []
    n = len(text)
    i = 0
    line = 1
    for _ in range(n):
        if i >= n:
            break
        c = text[i]
        if c == "\n":
            line += 1
            i += 1
        elif c in " \t\r":
            i += 1
        elif c == "#" or text[i:i + 2] == "//":
            end = text.find("\n", i)
            i = n if end < 0 else end
        elif text[i:i + 2] == "/*":
            end = text.find("*/", i + 2)
            if end < 0:
                return struct(tokens = None, error = "line {}: unterminated comment".format(line))
            line += text.count("\n", i, end)
            i = end + 2
        elif c in _PUNCTUATION:
            tokens.append(("p", c, line))
            i += 1
        elif c == "\"":
            chars = []
            closed = False
            j = i + 1
            for _ in range(n - j):
                if j >= n:
                    break
                d = text[j]
                if d == "\"":
                    closed = True
                    break
                if d == "\n":
                    break
                if d == "\\" and j + 1 < n and text[j + 1] in _ESCAPES:
                    chars.append(_ESCAPES[text[j + 1]])
                    j += 2
                    continue
                chars.append(d)
                j += 1
            if not closed:
                return struct(tokens = None, error = "line {}: unterminated string".format(line))
            tokens.append(("s", "".join(chars), line))
            i = j + 1
        elif c in _IDENT_CHARS:
            j = i
            for _ in range(n - i):
                if j >= n or text[j] not in _IDENT_CHARS:
                    break
                j += 1
            tokens.append(("i", text[i:j], line))
            i = j
        else:
            return struct(tokens = None, error = "line {}: unexpected character {}".format(line, repr(c)))
    return struct(tokens = tokens, error = None)

def _describe(token):
    if token == None:
        return "end of file"
    kind, value, line = token
    shown = repr(value) if kind == "s" else "`{}`".format(value)
    return "{} on line {}".format(shown, line)

def _parse_blocks(tokens):
    """Parse the token stream into `struct(address, attrs, line)` blocks."""
    blocks = []
    n = len(tokens)
    pos = 0

    def at(k):
        return tokens[k] if k < n else None

    for _ in range(n):
        if pos >= n:
            break
        head = at(pos)
        label = at(pos + 1)
        brace = at(pos + 2)
        if head[0] != "i" or head[1] != "provider":
            return struct(blocks = None, error = "expected a `provider` block, got {}".format(_describe(head)))
        if label == None or label[0] != "s":
            return struct(blocks = None, error = "expected the provider address after {}, got {}".format(_describe(head), _describe(label)))
        if brace == None or brace[:2] != ("p", "{"):
            return struct(blocks = None, error = "expected `{{` after {}, got {}".format(_describe(label), _describe(brace)))
        pos += 3
        attrs = {}
        closed = False
        for _ in range(n):
            token = at(pos)
            if token == None:
                break
            if token[:2] == ("p", "}"):
                closed = True
                pos += 1
                break
            equals = at(pos + 1)
            if token[0] != "i" or equals == None or equals[:2] != ("p", "="):
                return struct(blocks = None, error = "expected `<name> =` in the block for {}, got {}".format(repr(label[1]), _describe(token)))
            pos += 2
            value = at(pos)
            if value == None:
                break
            if value[0] == "s":
                attrs[token[1]] = value[1]
                pos += 1
            elif value[:2] == ("p", "["):
                items = []
                pos += 1
                list_closed = False
                for _ in range(n):
                    item = at(pos)
                    if item == None:
                        break
                    if item[:2] == ("p", "]"):
                        list_closed = True
                        pos += 1
                        break
                    if item[0] != "s":
                        return struct(blocks = None, error = "expected a string in `{}`, got {}".format(token[1], _describe(item)))
                    items.append(item[1])
                    pos += 1
                    separator = at(pos)
                    if separator != None and separator[:2] == ("p", ","):
                        pos += 1
                    elif separator == None or separator[:2] != ("p", "]"):
                        return struct(blocks = None, error = "expected `,` or `]` in `{}`, got {}".format(token[1], _describe(separator)))
                if not list_closed:
                    break
                attrs[token[1]] = items
            else:
                return struct(blocks = None, error = "expected a string or a list for `{}`, got {}".format(token[1], _describe(value)))
        if not closed:
            return struct(blocks = None, error = "the block for {} on line {} is not closed".format(repr(label[1]), head[2]))
        blocks.append(struct(address = label[1], attrs = attrs, line = head[2]))
    return struct(blocks = blocks, error = None)

def parse_lock(text):
    """Read the provider pins out of a `.terraform.lock.hcl`.

    An entry whose address isn't on the OpenTofu registry is still returned,
    with `error` set, so the extension can name it and point at `pin`.

    Args:
        text: The lock file's contents.

    Returns:
        `struct(entries, error)`. `error` is set if the file can't be read at
        all. Each entry is `struct(source, namespace, name, version, zh,
        error)`: `source` is the lower-cased `<namespace>/<name>`, `zh` the
        lock's `zh:` hashes (the SHA256s of the provider zips), and `error`
        None if the entry is usable.
    """
    tokenized = _tokenize(text)
    if tokenized.error:
        return struct(entries = None, error = tokenized.error)
    parsed = _parse_blocks(tokenized.tokens)
    if parsed.error:
        return struct(entries = None, error = parsed.error)

    entries = []
    seen = {}
    for block in parsed.blocks:
        parts = block.address.lower().split("/")
        if len(parts) != 3 or not parts[0] or not parts[1] or not parts[2]:
            return struct(
                entries = None,
                error = "line {}: provider address {} is not `<host>/<namespace>/<name>`".format(block.line, repr(block.address)),
            )
        host, namespace, name = parts
        source = "{}/{}".format(namespace, name)
        if source in seen:
            return struct(entries = None, error = "line {}: {} is locked more than once".format(block.line, repr(block.address)))
        seen[source] = True

        version = block.attrs.get("version")
        hashes = block.attrs.get("hashes", [])
        if type(version) != "string" or not version:
            return struct(entries = None, error = "line {}: {} has no `version`".format(block.line, repr(block.address)))
        if type(hashes) != "list":
            return struct(entries = None, error = "line {}: `hashes` of {} is not a list".format(block.line, repr(block.address)))

        zh = []
        for h in hashes:
            if h.startswith("zh:") and is_sha256(h[3:]):
                zh.append(h[3:])

        error = None
        if host != DEFAULT_HOST:
            error = ("{} is locked against {}, but rules_tofu downloads providers from {}, whose " +
                     "builds have different checksums. Lock it with OpenTofu instead.").format(
                block.address,
                host,
                DEFAULT_HOST,
            )
        elif not zh:
            error = "{} has no `zh:` hashes, so there is nothing to check its downloads against".format(block.address)
        entries.append(struct(
            source = source,
            namespace = namespace,
            name = name,
            version = version,
            zh = zh,
            error = error,
        ))
    return struct(entries = entries, error = None)

def lock_sha256s(registry_sha256s, zh, context):
    """Keep the registry's digests that the lock vouches for.

    The registry says where each zip is and what its SHA256 is; the lock's
    `zh:` hashes are what the user (or their bot) recorded when they last
    locked. Each platform is accepted only if both agree. Lock files list
    the `zh:` hash of every platform the provider publishes, whichever
    platforms they were locked for, so any disagreement means the lock and
    the registry describe different builds.

    Args:
        registry_sha256s: `{platform: sha256}` from the registry.
        zh: The lock entry's `zh:` hashes.
        context: `<source>@<version>`, for messages.

    Returns:
        `struct(sha256s, error)`; `error` is None on success.
    """
    locked = {h: True for h in zh}
    mismatched = sorted([p for p, digest in registry_sha256s.items() if digest not in locked])
    if mismatched:
        return struct(
            sha256s = None,
            error = ("the registry's zip for {} on {} matches none of the lock's `zh:` hashes. " +
                     "The lock describes different builds; regenerate it.").format(
                context,
                ", ".join(mismatched),
            ),
        )
    return struct(sha256s = dict(registry_sha256s), error = None)
