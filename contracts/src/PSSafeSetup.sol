// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

import {ISafeModuleTarget} from "./interfaces/ISafeModuleTarget.sol";
import {PSEscrowModule} from "./PSEscrowModule.sol";

/// Safe.setup の `to` / `data` に渡し、setup から delegatecall させる。
/// Safe の文脈で動くので、`address(this)` は作られたばかりの Safe。
contract PSSafeSetup {
    address private immutable SELF;

    error OnlyDelegateCall();

    constructor() {
        SELF = address(this);
    }

    function setup(address module, address token, address user, address shopper, uint64 t1, uint64 t2) external {
        if (address(this) == SELF) revert OnlyDelegateCall();
        // enableModule は `authorized`（msg.sender == Safe）なので、Safe から自分自身を呼ぶ。
        ISafeModuleTarget(address(this)).enableModule(module);
        PSEscrowModule(module).register(token, user, shopper, t1, t2);
    }
}
