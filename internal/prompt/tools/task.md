Starts a subagent: a separate agent with a fresh context that handles one bounded task on its own and reports back. It doesn't see this conversation, so the prompt must give it everything it needs: the goal, what you already know, the files involved and what its report should contain.

tools chooses what it may do:
- "read-only" (the default): read, search and list files. Use it to explore when that takes many searches: to find where something lives, how something works or what calls what.
- "full": also edit files and run commands, asking the user wherever the approval mode says to. Use it for a self-contained change.

When NOT to use it:
- To read a file whose path you know, use read_file; to find a file by its name, use glob.
- To find a specific definition, such as "func Parse", use grep.
- To search within two or three known files, read them yourself.
- For work your very next step depends on: do it yourself.

Usage notes:
- Give a short description (3-5 words); the user sees it.
- Delegate only concrete, bounded tasks that can run on their own, and don't duplicate work between yourself and a subagent.
- Several task calls in one reply run side by side, as many at a time as the configuration allows, the rest waiting their turn. Group them only when they are independent, and give subagents that edit separate files to change.
- The subagent's report comes to you, not to the user: relay what matters in your reply.
- Treat the report as evidence, not as certainly right: check its claims and review its changes before relying on them.
