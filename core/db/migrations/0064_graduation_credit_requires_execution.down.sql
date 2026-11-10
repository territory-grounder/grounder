-- Reverting removes the grounding check: credit becomes writable for an incident with no recorded execution
-- again, which is the state TG-321 describes. The trigger is dropped before the function it calls.
DROP TRIGGER IF EXISTS graduation_credit_grounded ON graduation_credit;
DROP FUNCTION IF EXISTS graduation_credit_requires_execution();
