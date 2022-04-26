package web3ext

// BorJs bor related apis
const BorJs = `
web3._extend({
	property: 'bor',
	methods: [
		new web3._extend.Method({
			name: 'getSnapshot',
			call: 'bor_getSnapshot',
			params: 1,
			inputFormatter: [null]
		}),
		new web3._extend.Method({
			name: 'getAuthor',
			call: 'bor_getAuthor',
			params: 1,
			inputFormatter: [null]
		}),
		new web3._extend.Method({
			name: 'getSnapshotAtHash',
			call: 'bor_getSnapshotAtHash',
			params: 1
		}),
		new web3._extend.Method({
			name: 'getSigners',
			call: 'bor_getSigners',
			params: 1,
			inputFormatter: [null]
		}),
		new web3._extend.Method({
			name: 'getSignersAtHash',
			call: 'bor_getSignersAtHash',
			params: 1
		}),
		new web3._extend.Method({
			name: 'getCurrentProposer',
			call: 'bor_getCurrentProposer',
			params: 0
		}),
		new web3._extend.Method({
			name: 'getCurrentValidators',
			call: 'bor_getCurrentValidators',
			params: 0
		}),
		new web3._extend.Method({
			name: 'getRootHash',
			call: 'bor_getRootHash',
			params: 2,
		}),
		new web3._extend.Method({
			name: 'writeBorTransaction',
			call: 'bor_writeBorTransaction',
			params: 3,
			inputFormatter: [null]
		}),
	]
});
`
