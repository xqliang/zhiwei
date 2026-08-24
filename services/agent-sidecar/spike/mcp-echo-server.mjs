#!/usr/bin/env node
/**
 * Trivial MCP server over stdio for the spike: exposes one `echo` tool.
 * Spawned by @deepseek-ai/dsh-mcp-client (the harness MCP client bridge), which
 * publishes the tool to the model as `mcp__<serverName>__echo`.
 *
 * Uses the low-level Server API with a plain JSON-Schema inputSchema (no zod in
 * our own code). MCP protocol frames ride THIS process's stdio, which is a
 * private channel between mcp-client and this server — unrelated to the harness
 * bin's own JSON-RPC stdout.
 */
import { Server } from '@modelcontextprotocol/sdk/server/index.js'
import { StdioServerTransport } from '@modelcontextprotocol/sdk/server/stdio.js'
import { ListToolsRequestSchema, CallToolRequestSchema } from '@modelcontextprotocol/sdk/types.js'

const server = new Server(
  { name: 'zhiwei-echo', version: '0.0.1' },
  { capabilities: { tools: {} } },
)

server.setRequestHandler(ListToolsRequestSchema, async () => ({
  tools: [
    {
      name: 'echo',
      description: 'Echo back the given text verbatim, prefixed with "ECHO: ". Use this to repeat text.',
      inputSchema: {
        type: 'object',
        properties: { text: { type: 'string', description: 'The text to echo back.' } },
        required: ['text'],
      },
    },
  ],
}))

server.setRequestHandler(CallToolRequestSchema, async (req) => {
  const text = req.params?.arguments?.text ?? ''
  // stderr is safe for diagnostics (stdout is the MCP wire for this process).
  process.stderr.write(`[mcp-echo] tools/call echo text=${JSON.stringify(text)}\n`)
  return { content: [{ type: 'text', text: `ECHO: ${text}` }] }
})

await server.connect(new StdioServerTransport())
process.stderr.write('[mcp-echo] connected over stdio\n')
