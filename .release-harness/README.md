# .release-harness

This directory holds your release contracts. release-harness created it and
made no claims about your software -- everything below is authored by you.

    drafts/     What you are still working out. Mutable; edit freely.
    accepted/   What you have committed to. Each file is named by its own
                digest, so a contract cannot be changed without becoming a
                different contract.
    bindings/   Where to go and look: ports, URLs, commands. Deliberately NOT
                part of any promise, so the same contract can be checked
                locally and in CI without its identity changing.
    runs/       What happened, sealed.

Start with:

    release-harness draft new <name>

That writes an empty draft for you to fill in. release-harness will not guess
what your software does; if it did, you would have no way to tell its guesses
apart from your own knowledge.
