// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

import {Test} from "forge-std/Test.sol";

import {MockUSDC} from "../src/mocks/MockUSDC.sol";
import {IERC20} from "../src/interfaces/IERC20.sol";
import {PSBond} from "../examples/bond/PSBond.sol";

contract PSBondTest is Test {
    MockUSDC usdc;
    PSBond bond;

    address operator = makeAddr("operator");
    address escrow = makeAddr("escrow");
    address victim = makeAddr("victim");
    uint64 constant DELAY = 7 days;

    function setUp() public {
        usdc = new MockUSDC();
        bond = new PSBond(IERC20(address(usdc)), operator, DELAY);
        usdc.mint(escrow, 1_000e6);
        vm.prank(escrow);
        usdc.approve(address(bond), type(uint256).max);
    }

    function test_depositAndSlash() public {
        vm.expectEmit(address(bond));
        emit PSBond.Deposited(escrow, 500e6, 500e6);
        vm.prank(escrow);
        bond.deposit(500e6);
        assertEq(bond.bondOf(escrow), 500e6);

        vm.prank(escrow);
        vm.expectRevert(PSBond.Unauthorized.selector);
        bond.slash(escrow, victim, 1);

        vm.prank(operator);
        vm.expectRevert(PSBond.InsufficientBond.selector);
        bond.slash(escrow, victim, 500e6 + 1);

        vm.expectEmit(address(bond));
        emit PSBond.Slashed(escrow, victim, 200e6, 300e6);
        vm.prank(operator);
        bond.slash(escrow, victim, 200e6);
        assertEq(usdc.balanceOf(victim), 200e6);
        assertEq(bond.bondOf(escrow), 300e6);
    }

    function test_withdrawAfterDelay_slashableMeanwhile() public {
        vm.startPrank(escrow);
        bond.deposit(500e6);
        vm.expectRevert(PSBond.NotRequested.selector);
        bond.withdraw();
        bond.requestWithdraw();
        vm.expectRevert(PSBond.TooEarly.selector);
        bond.withdraw();
        vm.stopPrank();

        vm.prank(operator);
        bond.slash(escrow, victim, 100e6);

        vm.warp(block.timestamp + DELAY);
        vm.expectEmit(address(bond));
        emit PSBond.Withdrawn(escrow, 400e6);
        vm.prank(escrow);
        bond.withdraw();
        assertEq(usdc.balanceOf(escrow), 900e6);
        assertEq(bond.bondOf(escrow), 0);
    }
}
