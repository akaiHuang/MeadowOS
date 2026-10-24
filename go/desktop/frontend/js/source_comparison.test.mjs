import assert from 'node:assert/strict';
import test from 'node:test';
import { sourceComparisonPresentation } from './source_comparison.js';

const row = {claim:'退貨7天',source_id:'doc-observed',start_line:1,end_line:2,quote:'不是7天',source_text:'期限14天，不是7天。',status:'literal_match',matched_keywords:['7天'],missing_keywords:['30天']};
const receipt = (checks=[row]) => ({type:'source_comparison',version:1,checks});

test('matching occurrence retains original negation and never displays a truth percentage',()=>{
  const result=sourceComparisonPresentation(JSON.stringify(receipt()));
  assert.equal(result.checks[0].sourceText,row.source_text);
  assert.match(result.notice,/不代表主張正確/);
  assert.equal(result.summary,'來源比對 · 1 項 · 文字找到 1 項');
  assert.deepEqual(result.checks[0].missing,['30天']);
  assert.match(result.coordinateScope,/並非 PDF/);
  assert.equal(result.accuracy,undefined);
});
test('comparison URLs are inert and input HTML remains plain text',()=>{
  for(const unsafe of ['javascript:alert(1)','data:text/html,<script>1</script>','https://user:secret@example.com/']){
    const result=sourceComparisonPresentation(receipt([{...row,claim:'<img onerror=1>',source_url:unsafe}]));
    assert.equal(result.checks[0].url,null);
    assert.equal(result.checks[0].claim,'<img onerror=1>');
  }
});
test('malformed, forged status and oversized comparisons are not promoted to cards',()=>{
  for(const bad of ['not json',receipt([]),receipt(Array(13).fill(row)),receipt([{...row,status:'verified_true'}]),receipt([{...row,source_text:'x'.repeat(4001)}]),receipt([{...row,end_line:81}]),receipt([{...row,missing_keywords:Array(9).fill('x')}]),{...receipt(),version:2}]){
    assert.equal(sourceComparisonPresentation(bad),null);
  }
});
test('unobserved evidence remains visibly unchecked with no original text',()=>{
  const result=sourceComparisonPresentation(receipt([{...row,status:'unobserved',source_text:'',quote:''}]));
  assert.equal(result.checks[0].label,'本輪未讀取來源');
  assert.match(result.summary,/文字找到 0 項/);
});
