"""Shared strict JSON decoding for one-shot and persistent forecast transports."""
import json

MAX_INPUT = 1024 * 1024


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate JSON field")
        result[key] = value
    return result


def reject_constant(_):
    raise ValueError("non-finite JSON value")


def decode_request(raw):
    if len(raw.encode("utf-8")) > MAX_INPUT:
        raise ValueError("forecast input exceeds limit")
    return json.loads(raw, object_pairs_hook=unique_object, parse_constant=reject_constant)


def read_message(stream):
    """Read one bounded NDJSON frame; drain oversized frames to the next newline.

    The limit counts UTF-8 payload bytes, excluding the line delimiter. Reading
    bounded chunks also prevents an unterminated oversized frame from growing a
    Python buffer indefinitely. The caller emits one error and can read again.
    """
    line = stream.readline(MAX_INPUT + 2)
    if line == "":
        return None
    if len(line) > MAX_INPUT and not line.endswith("\n"):
        while line and not line.endswith("\n"):
            line = stream.readline(MAX_INPUT + 2)
        raise ValueError("forecast input exceeds limit")
    message = line[:-1] if line.endswith("\n") else line
    if line.endswith("\n") and message.endswith("\r"):
        message = message[:-1]
    if len(message.encode("utf-8")) > MAX_INPUT:
        raise ValueError("forecast input exceeds limit")
    return message
