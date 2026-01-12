# Documentation Style Guide

## Tone and Voice

### Writing Principles
- **Direct and concise**: Get straight to the point without unnecessary preamble
- **Active voice**: Use "you" instead of "the user" (e.g., "Install the controller" not "The user should install the controller")
- **Natural language**: Write like an experienced developer explaining to another developer
- **Avoid AI-speak**: No overly formal or verbose phrasing that sounds generated

### Voice Examples
**❌ AVOID (AI-generated tone):**
"This comprehensive documentation provides detailed instructions for the implementation and configuration of the homelab autoscaler system, which facilitates the dynamic scaling of Kubernetes clusters based on workload demands through sophisticated state management."

**✅ PREFER (direct tone):**
"Scale your homelab Kubernetes cluster dynamically based on workload demands. The autoscaler manages physical node power states using wake-on-LAN, IPMI, or BMC interfaces."

## Structure and Organization

### Document Structure
Every document should follow this pattern:

1. **Title and brief purpose** (1-2 sentences)
2. **Prerequisites** (if applicable, bullet points)
3. **Main content** (step-by-step or detailed explanation)
4. **Examples** (concrete usage)
5. **Troubleshooting** (common issues and solutions)
6. **Related resources** (links to other docs)

### Heading Hierarchy
- `#` Main title
- `##` Section headings
- `###` Sub-sections
- `####` Minor topics (use sparingly)

## Content Guidelines

### Language and Phrasing
- **Be specific**: Instead of "various methods" say "wake-on-LAN, IPMI, or BMC"
- **Avoid redundancy**: Don't repeat the same information in different ways
- **Use contractions**: "don't", "can't", "it's" (sounds more natural)
- **Eliminate filler**: Remove phrases like "it should be noted that", "as previously mentioned"

### Technical Level
- **Target audience**: Developers familiar with Kubernetes basics
- **Assume knowledge**: Don't explain basic Kubernetes concepts
- **Be precise**: Use exact command syntax and configurations
- **Link out**: Reference official Kubernetes docs instead of re-explaining

### Formatting
- **Code blocks**: Use triple backticks with language specification
- **Commands**: Show exact commands users should run
- **Bullet points**: Use for lists of items, prerequisites, or options
- **Avoid excessive formatting**: No decorative emojis or unnecessary styling

## Examples

### Before (AI-style)
"The Homelab Autoscaler represents a sophisticated Kubernetes operator solution that enables comprehensive autoscaling capabilities for homelab environments featuring physical node infrastructure. This powerful tool manages power states through multiple supported protocols including Wake-on-LAN, Intelligent Platform Management Interface (IPMI), and Baseboard Management Controller (BMC) interfaces."

### After (Direct style)
"Scale your homelab Kubernetes cluster dynamically. The autoscaler manages physical node power states using wake-on-LAN, IPMI, or BMC interfaces when workload demands change."

## Review Checklist

Before finalizing any documentation, check:
- [ ] Is the tone direct and conversational?
- [ ] Have I used "you" instead of "the user"?
- [ ] Are there any redundant phrases or explanations?
- [ ] Is the technical level appropriate for the audience?
- [ ] Are commands and examples accurate and testable?
- [ ] Have I eliminated AI-sounding phrasing?
- [ ] Is the structure clear and logical?

## Common Pitfalls to Avoid

1. **Over-explaining**: Don't explain basic Kubernetes concepts
2. **Meta-commentary**: Avoid talking about the document itself
3. **Formal language**: Use natural, technical English
4. **Redundancy**: Don't say the same thing multiple ways
5. **Excessive formatting**: Keep it clean and focused on content