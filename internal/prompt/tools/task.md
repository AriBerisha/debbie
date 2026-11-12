Starts an agent: a separate agent with a fresh context that handles one bounded task on its own and reports back. It doesn't see this conversation, so the prompt must give it everything it needs: the goal, what you already know, the files involved and what its report should contain. Write the prompt from what you know now; the agent can look things up itself, so don't research its task for it first.

tools chooses what it may do:
- "read-only" (the default): read, search and list files. Use it to explore when that takes many searches: to find where something lives, how something works or what calls what.
- "full": also edit files and run commands, asking the user wherever the approval mode says to. Use it for a self-contained change.

background chooses how it runs:
- false (the default): this call returns the agent's report when it finishes.
- true: this call returns at once, and you carry on or end your turn. The report reaches you by itself when the agent finishes, so never wait or poll for it. Where the program can't run agents in the background, it runs as if false.

When NOT to use it:
- To read a file whose path you know, use read_file; to find a file by its name, use glob.
- To find a specific definition, such as "func Parse", use grep.
- To search within two or three known files, read them yourself.
- For work your very next step depends on: do it yourself.
- For anything you could finish yourself in a handful of tool calls.

Usage notes:
- Give a short description (3-5 words); the user sees it.
- Delegate only concrete, bounded tasks that can run on their own, and don't duplicate work between yourself and an agent.
- Several task calls in one reply run side by side, as many at a time as the provider allows, the rest waiting their turn. Group them only when they are independent, and give agents that edit separate files to change.
- The agent's report comes to you, not to the user: relay what matters in your reply.
- Treat the report as evidence, not as certainly right: check its claims and review its changes before relying on them.
