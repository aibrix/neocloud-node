## Summary

Describe the problem and the behavior changed by this pull request.

## Related Issue

Fixes #

## Provider and Pod Behavior

List the affected providers and describe any change to Pod translation, lifecycle,
status, retry, or cleanup behavior. Write `None` if provider behavior is unchanged.

## Tests Run

List the exact commands, environments, and results. Explain why any relevant test
was not run.

## Documentation and Manifests

List documentation, examples, RBAC, or generated manifests updated by this change.
Write `None` if no update is needed.

## Paid-Resource Cleanup Risk

State whether this change or its verification can create paid cloud resources. If
yes, describe the cost guard, cleanup procedure, and how leftovers were checked.
Write `None` when there is no paid-resource risk.

## Checklist

- [ ] I added or updated tests, ran the relevant checks, and listed the results above (or explained why tests are not needed).
- [ ] I updated relevant documentation, examples, RBAC, and generated manifests (or explained why no update is needed).
- [ ] I described provider behavior changes above, and provider-specific behavior stays behind a provider adapter.
- [ ] Retries and controller restarts cannot create duplicate paid resources.
- [ ] Delete and failure paths have been considered, including lost responses and manual remote deletion.
- [ ] Unsupported Pod fields fail explicitly instead of being silently ignored.
- [ ] No credentials, private endpoints, customer identifiers, or paid resource IDs are included.
- [ ] Any real-provider test has a cost guard, cleanup procedure, and leftover-resource check documented above.
