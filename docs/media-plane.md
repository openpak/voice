# Media plane (design only, nothing built)

Why nothing is built: the only native participant type that exists today, the
original Xbox, keeps its own audio (console to console over XNet). Moving that
audio through any server needs two things we do not have (the voice codec and the
XNet peer key exchange, internal research notes section 6). Phone to
phone already works over WebRTC without a server (website `internal/voice`).

When it is built it is additive: new RPCs (`GetMediaPlan`, bridge attach), the
participant `transport`/`codecs` fields already recorded here decide the path.
Three modes: no media in the core (default), a WebRTC SFU for rooms above the
mesh ceiling, and a mixing bridge (16 kHz mono PCM internally; the adapter owns
its native codec, the core owns Opus and mixing). ICE servers and relays come from
the `connectivity` service, not from this one.
